// Copyright 2013 Örjan Persson
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package libspotify adds language bindings for spotify in Go. The
// libspotify C API package allows third-party developers to write applications
// that utilize the Spotify music streaming service.
package libspotify

/*
#cgo pkg-config: libspotify
#include <libspotify/api.h>
#include "libspotify.h"
*/
import "C"

import (
	"errors"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	ErrMissingApplicationKey = errors.New("spotify: application key is required")
)

// Config represents the configuration setup when creating a new session.
type Config struct {
	// ApplicationKey is required and can be acquired from developer.spotify.com.
	ApplicationKey []byte

	// ApplicationName is used to determine cache locations and user agent.
	ApplicationName string

	// UserAgent is used when communicating with Spotify. If left empty, it
	// will automatically be created based on ApplicationName.
	UserAgent string

	// CacheLocation defines were Spotify will write any cache
	// files. This includes tracks, browse results and coverarts.
	// Leave empty to disable.
	CacheLocation string

	// SettingsLocation defines where Spotify will write settings
	// and per-user cache items. This includes playlists etc. It
	// may be the same location as the CacheLocation.
	//
	// Note: this directory will not be automatically created.
	SettingsLocation string

	// CompressPlaylists, if enabled, will compress local copies
	// of playlists to reduce disk space usage.
	CompressPlaylists bool

	// DisablePlaylistMetadataCache disables metadata caches for
	// playlists. It reduces disk space usage at the expense of
	// needing to request metadata from Spotify backend when
	// loading lists.
	DisablePlaylistMetadataCache bool

	// InitiallyUnloadPlaylists will avoid loading playlists into
	// RAM on startup if enabled.
	InitiallyUnloadPlaylists bool

	// TODO device_id
	// TODO proxy
	// TODO ca_certs
	// TODO tracefile
}

// Connection state describes the state of the connection of a session.
type ConnectionState C.sp_connectionstate

const (
	// User not yet logged in
	ConnectionStateLoggedOut ConnectionState = C.SP_CONNECTION_STATE_LOGGED_OUT

	// Logged in against an Spotify accesspoint
	ConnectionStateLoggedIn = C.SP_CONNECTION_STATE_LOGGED_IN

	// Was logged in, but has now been disconnected
	ConnectionStateDisconnected = C.SP_CONNECTION_STATE_DISCONNECTED

	// Connection state is undefined
	ConnectionStateUndefined = C.SP_CONNECTION_STATE_UNDEFINED

	// Logged in, but in offline mode
	ConnectionStateOffline = C.SP_CONNECTION_STATE_OFFLINE
)

var (
	// once is used to initiate the global state of the package.
	once sync.Once

	// callbacks is a static set of callbacks used for all sessions.
	callbacks C.sp_session_callbacks
)

// event is an internal type passed around to wake the main session thread up.
type event int

const (
	eWakeup event = iota
	eStop
)

// Credentials are used when logging a user in.
type Credentials struct {
	// Username is the spotify username.
	Username string

	// Password for the spotify username.
	Password string

	// Blob is an opaque data chunk used when logging in instead of password. If
	// login is successful and the remember flag set to true, this should be the
	// data blob retrieved from CredentialsBlobUpdates.
	Blob []byte
}

// Session is the representation of a Spotify session.
type Session struct {
	config     C.sp_session_config
	sp_session *C.sp_session
	mu         sync.Mutex

	events chan event

	credentialsBlobs chan []byte
	states           chan struct{}
	loggedIn         chan error
	loggedOut        chan struct{}

	wg      sync.WaitGroup
	dealloc sync.Once
}

// sessionCall maps the C Spotify session structure to the Go session and
// executes the given function.
func sessionCall(spSession unsafe.Pointer, callback func(*Session)) {
	s := (*C.sp_session)(spSession)
	session := (*Session)(C.sp_session_userdata(s))
	callback(session)
}

// NewSession creates a new session based on the given configuration.
func NewSession(config *Config) (*Session, error) {
	session := &Session{
		events: make(chan event, 1),

		credentialsBlobs: make(chan []byte, 1),
		states:           make(chan struct{}, 1),
		loggedIn:         make(chan error, 1),
		loggedOut:        make(chan struct{}, 1),
	}

	if err := session.setupConfig(config); err != nil {
		return nil, err
	}

	// libspotify expects certain methods to be called from the same thread as was
	// used when the sp_session_create was called. Hence we do lock down one
	// thread to only process events and some of these special calls.
	//
	// AFAIK this is the only way we can decide which thread a given goroutine
	// executes on.
	errc := make(chan error, 1)
	go func() {
		// TODO make sure we have enough threads available
		runtime.LockOSThread()

		err := spError(C.sp_session_create(&session.config, &session.sp_session))
		errc <- err
		if err != nil {
			return
		}
		session.processEvents()
	}()

	if err := <-errc; err != nil {
		return nil, err
	}

	return session, nil
}

// setupConfig sets the config up to be used when connecting the session.
func (s *Session) setupConfig(config *Config) error {
	if config.ApplicationKey == nil {
		return ErrMissingApplicationKey
	}

	s.config.api_version = C.SPOTIFY_API_VERSION

	s.config.cache_location = C.CString(config.CacheLocation)
	if s.config.cache_location == nil {
		return syscall.ENOMEM
	}

	s.config.settings_location = C.CString(config.SettingsLocation)
	if s.config.settings_location == nil {
		return syscall.ENOMEM
	}

	appKey := C.CString(string(config.ApplicationKey))
	s.config.application_key = unsafe.Pointer(appKey)
	if s.config.application_key == nil {
		return syscall.ENOMEM
	}
	s.config.application_key_size = C.size_t(len(config.ApplicationKey))

	userAgent := config.UserAgent
	if len(userAgent) == 0 {
		userAgent = "go-libspotify"
		if len(config.ApplicationName) > 0 {
			userAgent += "/" + config.ApplicationName
		}
	}
	s.config.user_agent = C.CString(userAgent)
	if s.config.user_agent == nil {
		return syscall.ENOMEM
	}

	// Setup the callbacks structure used for all sessions. The difference
	// between each session object is the userdata object which points into the
	// Go Session object.
	once.Do(func() { C.set_callbacks(&callbacks) })
	s.config.callbacks = &callbacks
	s.config.userdata = unsafe.Pointer(s)

	if config.CompressPlaylists {
		s.config.compress_playlists = 1
	}
	if config.DisablePlaylistMetadataCache {
		s.config.dont_save_metadata_for_playlists = 1
	}
	if config.InitiallyUnloadPlaylists {
		s.config.initially_unload_playlists = 1
	}
	return nil
}

func (s *Session) free() {
	if s.config.cache_location != nil {
		C.free(unsafe.Pointer(s.config.cache_location))
		s.config.cache_location = nil
	}
	if s.config.settings_location != nil {
		C.free(unsafe.Pointer(s.config.settings_location))
		s.config.settings_location = nil
	}
	if s.config.application_key != nil {
		C.free(unsafe.Pointer(s.config.application_key))
		s.config.application_key = nil
	}
	if s.config.user_agent != nil {
		C.free(unsafe.Pointer(s.config.user_agent))
		s.config.user_agent = nil
	}
}

// Close closes the session, making the session unusable for any future calls.
// This call releases the session internally back to libspotify and shuts the
// background processing thread down.
func (s *Session) Close() error {
	var err error
	s.dealloc.Do(func() {
		err = spError(C.sp_session_release(s.sp_session))

		s.events <- eStop
		s.wg.Wait()

		s.free()
	})
	return nil
}

// Login logs the the specified username and password combo. This
// initiates the login in the background.
//
// An application MUST NEVER store the user's password in clear
// text. If automatic relogin is required, use Relogin.
func (s *Session) Login(c Credentials, remember bool) error {
	cusername := C.CString(c.Username)
	defer C.free(unsafe.Pointer(cusername))
	var crememberme C.bool = 0
	if remember {
		crememberme = 1
	}
	var cpassword, cblob *C.char
	if len(c.Password) > 0 {
		cpassword = C.CString(c.Password)
		defer C.free(unsafe.Pointer(cpassword))
	}
	if len(c.Blob) > 0 {
		cblob = C.CString(string(c.Blob))
		defer C.free(unsafe.Pointer(cblob))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	rc := C.sp_session_login(
		s.sp_session,
		cusername,
		cpassword,
		crememberme,
		cblob,
	)
	return spError(rc)
}

// Relogin logs the remembered user in if the last user which logged in, logged
// in with the remember flag set to true.
//
// If no credentials are stored, this will return ErrNoCredentials.
func (s *Session) Relogin() error {
	return spError(C.sp_session_relogin(s.sp_session))
}

// Logout logs the currently logged in user out
//
// Always call this before terminating the application and
// libspotify is currently logged in. Otherwise, the settings and
// cache may be lost.
func (s *Session) Logout() error {
	return spError(C.sp_session_logout(s.sp_session))
}

// FlushCaches makes libspotify write all data that is meant to
// be stored on disk to the disk immediately. libspotify does this
// periodically by itself and also on logout. Under normal
// conditions this shouldn't be needed.
func (s *Session) FlushCaches() error {
	return spError(C.sp_session_flush_caches(s.sp_session))
}

// ConnectionState returns the current connection state for the
// session.
func (s *Session) ConnectionState() ConnectionState {
	state := C.sp_session_connectionstate(s.sp_session)
	return ConnectionState(state)
}

func (s *Session) ArtistsToplist(region ToplistRegion) *ArtistsToplist {
	return newArtistsToplist(s, region, "")
}

func (s *Session) ArtistsToplistForUser(username string) *ArtistsToplist {
	// TODO replace username string with a *User, make a
	// NewUser(string) method instead. Even move Toplist()
	// method on to the User object?
	return newArtistsToplist(s, ToplistRegionUser, username)
}

// CredentialsBlobUpdates returns a channel used to get updates
// for credential blobs.
func (s *Session) CredentialsBlobUpdates() <-chan []byte {
	return s.credentialsBlobs
}

// ConnectionStateUpdates returns a channel used to get updates on
// the connection state.
func (s *Session) ConnectionStateUpdates() <-chan struct{} {
	return s.states
}

// LoginUpdates returns a channel used to get notified when the
// session has been logged in.
func (s *Session) LoginUpdates() <-chan error {
	return s.loggedIn
}

// LogoutUpdates returns a channel used to get notified when the
// session has been logged out.
func (s *Session) LogoutUpdates() <-chan struct{} {
	return s.loggedOut
}

type SearchType C.sp_search_type

const (
	SearchStandard SearchType = SearchType(C.SP_SEARCH_STANDARD)
	SearchSuggest             = SearchType(C.SP_SEARCH_SUGGEST)
)

type SearchSpec struct {
	// Search result offset
	Offset int

	// Search result limitation
	Count int
}

// SearchOptions contains offsets and limits for the search query.
type SearchOptions struct {
	// Track is the number of tracks to search for
	Track SearchSpec

	// Album is the number of albums to search for
	Album SearchSpec

	// Artist is the number of artists to search for
	Artist SearchSpec

	// Playlist is the number of playlists to search for
	Playlist SearchSpec

	// Type is the search type. Defaults to normal searching.
	Type SearchType
}

// Search searches Spotify for track, album, artist and / or playlists.
func (s *Session) Search(query string, opts *SearchOptions) *search {
	cquery := C.CString(query)
	defer C.free(unsafe.Pointer(cquery))

	s.mu.Lock()
	defer s.mu.Unlock()

	var search search
	sp_search := C.search_create(
		s.sp_session,
		cquery,
		C.int(opts.Track.Offset),
		C.int(opts.Track.Count),
		C.int(opts.Album.Offset),
		C.int(opts.Album.Count),
		C.int(opts.Artist.Offset),
		C.int(opts.Artist.Count),
		C.int(opts.Playlist.Offset),
		C.int(opts.Playlist.Count),
		C.sp_search_type(opts.Type),
		unsafe.Pointer(&search),
	)
	search.init(s, sp_search)
	return &search
}

func (s *Session) processEvents() {
	var nextTimeoutMs C.int

	s.wg.Add(1)
	defer s.wg.Done()

	for {
		s.mu.Lock()
		rc := C.sp_session_process_events(s.sp_session, &nextTimeoutMs)
		s.mu.Unlock()
		if err := spError(rc); err != nil {
			println("process error err", err)
			continue
		}

		timeout := time.Duration(nextTimeoutMs) * time.Millisecond
		select {
		case <-time.After(timeout):
		case evt := <-s.events:
			if evt == eStop {
				return
			}
		}
	}
}

func (s *Session) cbLoggedIn(err error) {
	println("logged in called", s, err)
	select {
	case s.loggedIn <- err:
	default:
		println("failed to send logged in event")
	}
}

func (s *Session) cbLoggedOut() {
	println("logged out called", s)
	select {
	case s.loggedOut <- struct{}{}:
	default:
		println("failed to send logged out event")
	}
}

func (s *Session) cbMetadataUpdated() {
	// TODO
	println("metadata updated")
}

func (s *Session) cbConnectionError(err error) {
	println("connection errror called", s, err)
}

func (s *Session) cbMessageToUser(message string) {
	// TODO
	println("message to user", message)
}

func (s *Session) cbNotifyMainThread() {
	select {
	case s.events <- eWakeup:
	default:
		println("failed to notify main thread")
		// TODO generate (internal) log message
	}
}

func (s *Session) cbPlayTokenLost() {
	// TODO
	println("play token lost")
}

func (s *Session) cbLogMessage(message string) {
	println("LOG", message)
}

func (s *Session) cbStreamingError(err error) {
	println("streaming error", err.Error())
}

func (s *Session) cbOfflineError(err error) {
	println("offline error", err.Error())
}

func (s *Session) cbCredentialsBlobUpdated(blob []byte) {
	select {
	case s.credentialsBlobs <- blob:
	default:
	}
}

func (s *Session) cbConnectionStateUpdated() {
	select {
	case s.states <- struct{}{}:
	default:
	}
}

func (s *Session) cbScrobbleError(err error) {
	println("scrobble error", err.Error())
}

func (s *Session) cbPrivateSessionModeChanged(private bool) {
	println("private mode changed", private)
}

//export go_logged_in
func go_logged_in(spSession unsafe.Pointer, spErr C.sp_error) {
	sessionCall(spSession, func(s *Session) {
		s.cbLoggedIn(spError(spErr))
	})
}

//export go_logged_out
func go_logged_out(spSession unsafe.Pointer) {
	sessionCall(spSession, (*Session).cbLoggedOut)
}

//export go_metadata_updated
func go_metadata_updated(spSession unsafe.Pointer) {
	sessionCall(spSession, (*Session).cbMetadataUpdated)
}

//export go_connection_error
func go_connection_error(spSession unsafe.Pointer, spErr C.sp_error) {
	sessionCall(spSession, func(s *Session) {
		s.cbConnectionError(spError(spErr))
	})
}

//export go_message_to_user
func go_message_to_user(spSession unsafe.Pointer, message *C.char) {
	sessionCall(spSession, func(s *Session) {
		s.cbMessageToUser(C.GoString(message))
	})
}

//export go_notify_main_thread
func go_notify_main_thread(spSession unsafe.Pointer) {
	sessionCall(spSession, (*Session).cbNotifyMainThread)
}

//export go_play_token_lost
func go_play_token_lost(spSession unsafe.Pointer) {
	sessionCall(spSession, (*Session).cbPlayTokenLost)
}

//export go_log_message
func go_log_message(spSession unsafe.Pointer, message *C.char) {
	sessionCall(spSession, func(s *Session) {
		s.cbLogMessage(C.GoString(message))
	})
}

//export go_streaming_error
func go_streaming_error(spSession unsafe.Pointer, err C.sp_error) {
	sessionCall(spSession, func(s *Session) {
		s.cbStreamingError(spError(err))
	})
}

//export go_offline_error
func go_offline_error(spSession unsafe.Pointer, err C.sp_error) {
	sessionCall(spSession, func(s *Session) {
		s.cbOfflineError(spError(err))
	})
}

//export go_credentials_blob_updated
func go_credentials_blob_updated(spSession unsafe.Pointer, data *C.char) {
	sessionCall(spSession, func(s *Session) {
		// We keep the blob as []byte instead of string because it just makes more
		// sense than how libspotify does it.
		blob := []byte(C.GoString(data))
		s.cbCredentialsBlobUpdated(blob)
	})
}

//export go_connectionstate_updated
func go_connectionstate_updated(spSession unsafe.Pointer) {
	sessionCall(spSession, (*Session).cbConnectionStateUpdated)
}

//export go_scrobble_error
func go_scrobble_error(spSession unsafe.Pointer, err C.sp_error) {
	sessionCall(spSession, func(s *Session) {
		s.cbScrobbleError(spError(err))
	})
}

//export go_private_session_mode_changed
func go_private_session_mode_changed(spSession unsafe.Pointer, is_private C.bool) {
	sessionCall(spSession, func(s *Session) {
		s.cbPrivateSessionModeChanged(is_private == 1)
	})
}

//export go_search_complete
func go_search_complete(spSearch unsafe.Pointer, userdata unsafe.Pointer) {
	s := (*search)(userdata)
	s.cbComplete()
}

//export go_toplistbrowse_complete
func go_toplistbrowse_complete(sp_toplistsearch unsafe.Pointer, userdata unsafe.Pointer) {
	// TODO find a nicer way to do this
	t := (*toplist)(userdata)
	switch t.ttype {
	case toplistTypeArtists:
		((*ArtistsToplist)(userdata)).cbComplete()
	case toplistTypeAlbums:
		((*AlbumsToplist)(userdata)).cbComplete()
	case toplistTypeTracks:
		((*TracksToplist)(userdata)).cbComplete()
	default:
		panic("spotify: unhandled toplist type")
	}
}

type LinkType C.sp_linktype

const (
	// Link type not valid - default until the library has parsed the link, or
	// when parsing failed
	LinkTypeInvalid = LinkType(C.SP_LINKTYPE_INVALID)
	// Link type is track
	LinkTypeTrack = LinkType(C.SP_LINKTYPE_TRACK)
	// Link type is album
	LinkTypeAlbum = LinkType(C.SP_LINKTYPE_ALBUM)
	// Link type is artist
	LinkTypeArtist = LinkType(C.SP_LINKTYPE_ARTIST)
	// Link type is search
	LinkTypeSearch = LinkType(C.SP_LINKTYPE_SEARCH)
	// Link type is playlist
	LinkTypePlaylist = LinkType(C.SP_LINKTYPE_PLAYLIST)
	// Link type is profile
	LinkTypeProfile = LinkType(C.SP_LINKTYPE_PROFILE)
	// Link type is starred
	LinkTypeStarred = LinkType(C.SP_LINKTYPE_STARRED)
	// Link type is a local file
	LinkTypeLocalTrack = LinkType(C.SP_LINKTYPE_LOCALTRACK)
	// Link type is an image
	LinkTypeImage = LinkType(C.SP_LINKTYPE_IMAGE)
)

type Link struct {
	sp_link *C.sp_link
}

func NewLink(link string) (*Link, error) {
	clink := C.CString(link)
	defer C.free(unsafe.Pointer(clink))
	sp_link := C.sp_link_create_from_string(clink)
	if sp_link == nil {
		return nil, errors.New("spotify: invalid spotify link")
	}
	return newLink(sp_link, false), nil
}

func newLink(sp_link *C.sp_link, incRef bool) *Link {
	if incRef {
		C.sp_link_add_ref(sp_link)
	}
	link := &Link{sp_link}
	runtime.SetFinalizer(link, (*Link).release)
	return link
}

func (l *Link) release() {
	if l.sp_link == nil {
		panic("spotify: link object has no sp_link object")
	}
	C.sp_link_release(l.sp_link)
	l.sp_link = nil
}

// String implements the Stringer interface and returns the Link URI.
func (l *Link) String() string {
	// Determine how big string we need and get the string out.
	size := C.sp_link_as_string(l.sp_link, nil, 0)
	buf := (*C.char)(C.calloc(1, C.size_t(size)+1))
	if buf == nil {
		return "<invalid>"
	}
	defer C.free(unsafe.Pointer(buf))
	C.sp_link_as_string(l.sp_link, buf, size+1)
	return C.GoString(buf)
}

// LinkType returns the type of link.
func (l *Link) Type() LinkType {
	return LinkType(C.sp_link_type(l.sp_link))
}

func (l *Link) Track() (*Track, error) {
	if l.Type() != LinkTypeTrack {
		return nil, errors.New("spotify: link is not a track")
	}
	// HACK add session everywhere so we can reach this
	return newTrack(nil, C.sp_link_as_track(l.sp_link)), nil
}

// TrackOffset returns the offset for the track link.
func (l *Link) TrackOffset() time.Duration {
	var offsetMs C.int
	C.sp_link_as_track_and_offset(l.sp_link, &offsetMs)
	return time.Duration(offsetMs) / time.Millisecond
}

func (l *Link) Album() (*Album, error) {
	if l.Type() != LinkTypeAlbum {
		return nil, errors.New("spotify: link is not an album")
	}
	return newAlbum(C.sp_link_as_album(l.sp_link)), nil
}

func (l *Link) Artist() (*Artist, error) {
	if l.Type() != LinkTypeArtist {
		return nil, errors.New("spotify: link is not an artist")
	}
	return newArtist(C.sp_link_as_artist(l.sp_link)), nil
}

// TODO sp_link_as_user

type search struct {
	session   *Session
	sp_search *C.sp_search
	wg        sync.WaitGroup
}

func (s *search) init(session *Session, sp_search *C.sp_search) {
	s.session = session
	s.sp_search = sp_search
	s.wg.Add(1)
	runtime.SetFinalizer(s, (*search).release)
}

func (s *search) release() {
	if s.sp_search == nil {
		panic("spotify: search object has no sp_search object")
	}
	C.sp_search_release(s.sp_search)
	s.sp_search = nil
}

func (s *search) Wait() {
	s.wg.Wait()
}

func (s *search) Link() *Link {
	sp_link := C.sp_link_create_from_search(s.sp_search)
	return newLink(sp_link, false)
}

func (s *search) cbComplete() {
	s.wg.Done()
}

func (s *search) Error() error {
	return spError(C.sp_search_error(s.sp_search))
}

func (s *search) Query() string {
	return C.GoString(C.sp_search_query(s.sp_search))
}

func (s *search) DidYouMean() string {
	return C.GoString(C.sp_search_did_you_mean(s.sp_search))
}

func (s *search) Tracks() int {
	return int(C.sp_search_num_tracks(s.sp_search))
}

func (s *search) TotalTracks() int {
	return int(C.sp_search_total_tracks(s.sp_search))
}

func (s *search) Track(n int) *Track {
	if n < 0 || n >= s.Tracks() {
		panic("spotify: search track out of range")
	}
	sp_track := C.sp_search_track(s.sp_search, C.int(n))
	return newTrack(s.session, sp_track)
}

func (s *search) Albums() int {
	return int(C.sp_search_num_albums(s.sp_search))
}

func (s *search) TotalAlbums() int {
	return int(C.sp_search_total_albums(s.sp_search))
}

func (s *search) Artists() int {
	return int(C.sp_search_num_artists(s.sp_search))
}

func (s *search) TotalArtists() int {
	return int(C.sp_search_total_artists(s.sp_search))
}

func (s *search) Playlists() int {
	return int(C.sp_search_num_playlists(s.sp_search))
}

func (s *search) TotalPlaylists() int {
	return int(C.sp_search_total_playlists(s.sp_search))
}

type Track struct {
	session  *Session
	sp_track *C.sp_track
	wg       sync.WaitGroup
}

func newTrack(s *Session, t *C.sp_track) *Track {
	C.sp_track_add_ref(t)
	track := &Track{
		session:  s,
		sp_track: t,
	}
	runtime.SetFinalizer(track, (*Track).release)
	return track
}

func (t *Track) release() {
	if t.sp_track == nil {
		panic("spotify: track object has no sp_track object")
	}
	C.sp_track_release(t.sp_track)
	t.sp_track = nil
}

// Error returns an error associated with a track.
func (t *Track) Error() error {
	return spError(C.sp_track_error(t.sp_track))
}

func (t *Track) OfflineStatus() TrackOfflineStatus {
	status := C.sp_track_offline_get_status(t.sp_track)
	return TrackOfflineStatus(status)
}

// Availability returns the track availability.
func (t *Track) Availability() TrackAvailability {
	avail := C.sp_track_get_availability(
		t.session.sp_session,
		t.sp_track,
	)
	return TrackAvailability(avail)
}

// IsLocal returns true if the track is a local file.
func (t *Track) IsLocal() bool {
	local := C.sp_track_is_local(
		t.session.sp_session,
		t.sp_track,
	)
	return local == 1
}

// IsAutoLinked returns true if the track is auto-linked to another track.
func (t *Track) IsAutoLinked() bool {
	linked := C.sp_track_is_autolinked(
		t.session.sp_session,
		t.sp_track,
	)
	return linked == 1
}

// PlayableTrack returns the track which is the actual track that will be
// played if the given track is played.
func (t *Track) PlayableTrack() *Track {
	sp_track := C.sp_track_get_playable(
		t.session.sp_session,
		t.sp_track,
	)
	return newTrack(t.session, sp_track)
}

// IsPlaceholder returns true if the track is a placeholder. Placeholder tracks
// are used to store other objects than tracks in the playlist. Currently this
// is used in the inbox to store artists, albums and playlists.
//
// Use Link() to get a link object that points to the real object this "track"
// points to.
func (t *Track) IsPlaceholder() bool {
	placeholder := C.sp_track_is_placeholder(
		t.sp_track,
	)
	return placeholder == 1
}

// Link returns a link object representing the track.
func (t *Track) Link() *Link {
	return t.LinkOffset(0)
}

// Link returns a link object representing the track at the given offset.
func (t *Track) LinkOffset(offset time.Duration) *Link {
	offsetMs := C.int(offset / time.Millisecond)
	sp_link := C.sp_link_create_from_track(t.sp_track, offsetMs)
	return newLink(sp_link, false)
}

// IsStarred returns true if the track is starred by the currently logged in
// user.
func (t *Track) IsStarred() bool {
	starred := C.sp_track_is_starred(
		t.session.sp_session,
		t.sp_track,
	)
	return starred == 1
}

// TODO sp_track_set_starred

// Artists returns the number of artists performing on the track.
func (t *Track) Artists() int {
	return int(C.sp_track_num_artists(t.sp_track))
}

// Artist returns the artist on the specified index. Use Artists to know how
// many artists that performed on the track.
func (t *Track) Artist(n int) *Artist {
	if n < 0 || n >= t.Artists() {
		panic("spotify: track artist index out of range")
	}
	sp_artist := C.sp_track_artist(t.sp_track, C.int(n))
	return newArtist(sp_artist)
}

func (t *Track) Wait() {
	// TODO make this more elegant and based on callback
	for {
		if t.isLoaded() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (t *Track) isLoaded() bool {
	return C.sp_track_is_loaded(t.sp_track) == 1
}

// Album returns the album of the track.
func (t *Track) Album() *Album {
	sp_album := C.sp_track_album(t.sp_track)
	return newAlbum(sp_album)
}

// Name returns the track name.
func (t *Track) Name() string {
	return C.GoString(C.sp_track_name(t.sp_track))
}

// Duration returns the length of the current track.
func (t *Track) Duration() time.Duration {
	ms := C.sp_track_duration(t.sp_track)
	return time.Duration(ms) * time.Millisecond
}

// Popularity is in the range [0, 100].
type Popularity int

// Popularity returns the popularity for the track.
func (t *Track) Popularity() Popularity {
	p := C.sp_track_popularity(t.sp_track)
	return Popularity(p)
}

// Disc returns the disc number for the track.
func (t *Track) Disc() int {
	return int(C.sp_track_disc(t.sp_track))
}

// Position returns the position of a track on its disc.
// It starts at 1 (relative the corresponding disc).
//
// This function returns valid data only for tracks
// appearing in a browse artist or browse album result
// (otherwise returns 0).
func (t *Track) Index() int {
	return int(C.sp_track_index(t.sp_track))
}

// TODO sp_localtrack_create

type TrackAvailability C.sp_track_availability

const (
	// Track is not available
	TrackAvailabilityUnavailable = TrackAvailability(C.SP_TRACK_AVAILABILITY_UNAVAILABLE)

	// Track is available and can be played
	TrackAvailabilityAvailable = TrackAvailability(C.SP_TRACK_AVAILABILITY_AVAILABLE)

	// Track can not be streamed using this account
	TrackAvailabilityNotStreamable = TrackAvailability(C.SP_TRACK_AVAILABILITY_NOT_STREAMABLE)

	// Track not available on artist's request
	TrackAvailabilityBannedByArtist = TrackAvailability(C.SP_TRACK_AVAILABILITY_BANNED_BY_ARTIST)
)

type TrackOfflineStatus C.sp_track_offline_status

const (
	// Not marked for offline
	TrackOfflineNo = TrackOfflineStatus(C.SP_TRACK_OFFLINE_NO)
	// Waiting for download
	TrackOfflineWaiting = TrackOfflineStatus(C.SP_TRACK_OFFLINE_WAITING)
	// Currently downloading
	TrackOfflineDownloading = TrackOfflineStatus(C.SP_TRACK_OFFLINE_DOWNLOADING)
	// Downloaded OK and can be played
	TrackOfflineDone = TrackOfflineStatus(C.SP_TRACK_OFFLINE_DONE)
	// TrackOfflineStatus during download
	TrackOfflineTrackOfflineStatus = TrackOfflineStatus(C.SP_TRACK_OFFLINE_ERROR)
	// Downloaded OK but not playable due to expiery
	TrackOfflineDoneExpired = TrackOfflineStatus(C.SP_TRACK_OFFLINE_DONE_EXPIRED)
	// Waiting because device have reached max number of allowed tracks
	TrackOfflineLimitExceeded = TrackOfflineStatus(C.SP_TRACK_OFFLINE_LIMIT_EXCEEDED)
	// Downloaded OK and available but scheduled for re-download
	TrackOfflineDoneResync = TrackOfflineStatus(C.SP_TRACK_OFFLINE_DONE_RESYNC)
)

type Album struct {
	sp_album *C.sp_album
}

type AlbumType C.sp_albumtype

const (
	// Normal album
	AlbumTypeAlbum = AlbumType(C.SP_ALBUMTYPE_ALBUM)
	// Single
	AlbumTypeSingle = AlbumType(C.SP_ALBUMTYPE_SINGLE)
	// Compilation
	AlbumTypeCompilation = AlbumType(C.SP_ALBUMTYPE_COMPILATION)
	// Unknown type
	AlbumTypeUnknown = AlbumType(C.SP_ALBUMTYPE_UNKNOWN)
)

func newAlbum(sp_album *C.sp_album) *Album {
	C.sp_album_add_ref(sp_album)
	album := &Album{sp_album}
	runtime.SetFinalizer(album, (*Album).release)
	return album
}

func (a *Album) release() {
	if a.sp_album == nil {
		panic("spotify: album object has no sp_album object")
	}
	C.sp_album_release(a.sp_album)
	a.sp_album = nil
}

func (a *Album) Wait() {
	// TODO make perty
	for {
		if a.isLoaded() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Link creates a link object from the album.
func (a *Album) Link() *Link {
	sp_link := C.sp_link_create_from_album(a.sp_album)
	return newLink(sp_link, false)
}

// IsAvailable returns true if the album is available in the current region and
// for playback.
func (a *Album) IsAvailable() bool {
	return C.sp_album_is_available(a.sp_album) == 1
}

// TODO sp_album_artist
// TODO sp_album_cover
// TODO sp_link_create_from_album_cover

// Name returns the name of the album.
func (a *Album) Name() string {
	return C.GoString(C.sp_album_name(a.sp_album))
}

// Year returns the release year.
func (a *Album) Year() int {
	return int(C.sp_album_year(a.sp_album))
}

// Type returns the type of album.
func (a *Album) Type() AlbumType {
	return AlbumType(C.sp_album_type(a.sp_album))
}

func (a *Album) isLoaded() bool {
	return C.sp_album_is_loaded(a.sp_album) == 1
}

type Artist struct {
	sp_artist *C.sp_artist
}

func newArtist(sp_artist *C.sp_artist) *Artist {
	C.sp_artist_add_ref(sp_artist)
	artist := &Artist{sp_artist}
	runtime.SetFinalizer(artist, (*Artist).release)
	return artist
}

func (a *Artist) release() {
	if a.sp_artist == nil {
		panic("spotify: artist object has no sp_artist object")
	}
	C.sp_artist_release(a.sp_artist)
	a.sp_artist = nil
}

func (a *Artist) isLoaded() bool {
	return C.sp_artist_is_loaded(a.sp_artist) == 1
}

func (a *Artist) Wait() {
	// TODO make perty
	for {
		if a.isLoaded() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Link creates a link object from the artist.
func (a *Artist) Link() *Link {
	sp_link := C.sp_link_create_from_artist(a.sp_artist)
	return newLink(sp_link, false)
}

// Name returns the name of the artist.
func (a *Artist) Name() string {
	return C.GoString(C.sp_artist_name(a.sp_artist))
}

// TODO sp_artist_portrait
// TODO sp_link_create_from_artist_portrait

type RelationType C.sp_relation_type

const (
	// Not yet known
	RelationTypeUnknown = RelationType(C.SP_RELATION_TYPE_UNKNOWN)
	// No relation
	RelationTypeNone = RelationType(C.SP_RELATION_TYPE_NONE)
	// The currently logged in user is following this uer
	RelationTypeUnIdirectional = RelationType(C.SP_RELATION_TYPE_UNIDIRECTIONAL)
	// Bidirectional friendship established
	RelationTypeBidirectional = RelationType(C.SP_RELATION_TYPE_BIDIRECTIONAL)
)

type User struct {
	sp_user *C.sp_user
}

func newUser(sp_user *C.sp_user) *User {
	C.sp_user_add_ref(sp_user)
	user := &User{sp_user}
	// TODO make an inteface with release and some convenient func
	runtime.SetFinalizer(user, (*User).release)
	return user
}

func (u *User) release() {
	if u.sp_user == nil {
		panic("spotify: user object has no sp_user object")
	}
	C.sp_user_release(u.sp_user)
	u.sp_user = nil
}

func (u *User) Wait() {
	// TODO hook into the callback/event system
	for {
		if u.isLoaded() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (u *User) isLoaded() bool {
	return C.sp_user_is_loaded(u.sp_user) == 1
}

// CanonicalName returns the user's canonical username.
func (u *User) CanonicalName() string {
	return C.GoString(C.sp_user_canonical_name(u.sp_user))
}

// DisplayName returns the user's displayable username.
func (u *User) DisplayName() string {
	return C.GoString(C.sp_user_display_name(u.sp_user))
}

type toplistType C.sp_toplisttype

const (
	toplistTypeArtists = toplistType(C.SP_TOPLIST_TYPE_ARTISTS)
	toplistTypeAlbums  = toplistType(C.SP_TOPLIST_TYPE_ALBUMS)
	toplistTypeTracks  = toplistType(C.SP_TOPLIST_TYPE_TRACKS)
)

type ToplistRegion C.sp_toplistregion

const (
	// Global toplist
	ToplistRegionEverywhere = ToplistRegion(C.SP_TOPLIST_REGION_EVERYWHERE)

	// Toplist for the given user
	ToplistRegionUser = ToplistRegion(C.SP_TOPLIST_REGION_USER)
)

// NewToplistRegion returns the toplist region for a ISO
// 3166-1 country code.
//
// Also see ToplistRegionEverywhere and ToplistRegionUser
// for some special constants.
func NewToplistRegion(region string) ToplistRegion {
	if len(region) != 2 {
		panic("spotify: region should have length 2")
	}
	region = strings.ToUpper(region)
	r := int(region[0])<<8 | int(region[1])
	return ToplistRegion(r)
}

func (r ToplistRegion) String() string {
	switch r {
	case ToplistRegionEverywhere:
		return "Worldwide"
	case ToplistRegionUser:
		// TODO fetch users country?
		return "User"
	}
	return string([]byte{byte(r >> 8), byte(r)})
}

type toplist struct {
	session *Session

	sp_toplistbrowse *C.sp_toplistbrowse
	ttype            toplistType

	wg sync.WaitGroup
}

func newToplist(s *Session, ttype toplistType, r ToplistRegion, u string) *toplist {
	var cusername *C.char
	if len(u) > 0 {
		cusername = C.CString(u)
		defer C.free(unsafe.Pointer(cusername))
	}

	t := &toplist{session: s, ttype: ttype}
	t.wg.Add(1)
	t.sp_toplistbrowse = C.toplistbrowse_create(
		t.session.sp_session,
		C.sp_toplisttype(ttype),
		C.sp_toplistregion(r),
		cusername,
		unsafe.Pointer(&t),
	)
	runtime.SetFinalizer(t, (*toplist).release)
	return t
}

func (t *toplist) release() {
	if t.sp_toplistbrowse == nil {
		panic("spotify: toplist object has no sp_toplistbrowse object")
	}
	C.sp_toplistbrowse_release(t.sp_toplistbrowse)
	t.sp_toplistbrowse = nil
}

func (t *toplist) cbComplete() {
	println("toplist done", t)
	t.wg.Done()
}

func (t *toplist) Wait() {
	println("waiting for toplist", t)
	t.wg.Wait()
}

func (t *toplist) Error() error {
	return spError(C.sp_toplistbrowse_error(t.sp_toplistbrowse))
}

// Duration returns the time spent waiting for
// the Spotify backend to serve the toplist.
func (t *toplist) Duration() time.Duration {
	ms := C.sp_toplistbrowse_backend_request_duration(t.sp_toplistbrowse)
	if ms < 0 {
		ms = 0
	}
	return time.Duration(ms) * time.Millisecond
}

// TODO plural here, really?
type ArtistsToplist struct {
	*toplist
}

func newArtistsToplist(s *Session, r ToplistRegion, u string) *ArtistsToplist {
	toplist := newToplist(s, toplistTypeArtists, r, u)
	return &ArtistsToplist{toplist}
}

func (at *ArtistsToplist) Artists() int {
	return int(C.sp_toplistbrowse_num_artists(at.sp_toplistbrowse))
}

func (at *ArtistsToplist) Artist(n int) *Artist {
	if n < 0 || n >= at.Artists() {
		panic("spotify: toplist artist out of range")
	}
	sp_artist := C.sp_toplistbrowse_artist(at.sp_toplistbrowse, C.int(n))
	return newArtist(sp_artist)
}

// TODO
type AlbumsToplist struct {
	*toplist
}

func newAlbumsToplist(s *Session, r ToplistRegion, u string) *AlbumsToplist {
	toplist := newToplist(s, toplistTypeAlbums, r, u)
	return &AlbumsToplist{toplist}
}

func (at *AlbumsToplist) Albums() int {
	return int(C.sp_toplistbrowse_num_albums(at.sp_toplistbrowse))
}

func (at *AlbumsToplist) Album(n int) *Album {
	if n < 0 || n >= at.Albums() {
		panic("spotify: toplist album out of range")
	}
	sp_album := C.sp_toplistbrowse_album(at.sp_toplistbrowse, C.int(n))
	return newAlbum(sp_album)
}

type TracksToplist struct {
	*toplist
}

func newTracksToplist(s *Session, r ToplistRegion, u string) *TracksToplist {
	toplist := newToplist(s, toplistTypeTracks, r, u)
	return &TracksToplist{toplist}
}

// Tracks returns the numbers of tracks in the toplist.
func (tt *TracksToplist) Tracks() int {
	return int(C.sp_toplistbrowse_num_tracks(tt.sp_toplistbrowse))
}

// Track returns the track given the index from the toplist.
func (tt *TracksToplist) Track(n int) *Track {
	if n < 0 || n >= tt.Tracks() {
		panic("spotify: toplist track out of range")
	}
	sp_track := C.sp_toplistbrowse_track(tt.sp_toplistbrowse, C.int(n))
	return newTrack(tt.session, sp_track)
}

// BuildId returns the libspotify build ID.
func BuildId() string {
	return C.GoString(C.sp_build_id())
}
