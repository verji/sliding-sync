package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/sync-v3/internal"
	"github.com/matrix-org/sync-v3/state"
	"github.com/matrix-org/sync-v3/sync2"
	"github.com/matrix-org/sync-v3/sync3"
	"github.com/matrix-org/sync-v3/sync3/notifier"
	"github.com/matrix-org/sync-v3/sync3/store"
	"github.com/matrix-org/sync-v3/sync3/streams"
	"github.com/rs/zerolog/hlog"
	"github.com/rs/zerolog/log"
	"github.com/tidwall/gjson"
)

type SyncV3Handler struct {
	V2       sync2.Client
	Sessions *store.Sessions
	Storage  *state.Storage
	Notifier *notifier.Notifier

	PollerMap *sync2.PollerMap

	streams []streams.Streamer
}

func NewSyncV3Handler(v2Client sync2.Client, postgresDBURI string) *SyncV3Handler {
	sh := &SyncV3Handler{
		V2:       v2Client,
		Sessions: store.NewSessions(postgresDBURI),
		Storage:  state.NewStorage(postgresDBURI),
	}
	sh.PollerMap = sync2.NewPollerMap(v2Client, sh)
	sh.streams = append(sh.streams, streams.NewTyping(sh.Storage))
	sh.streams = append(sh.streams, streams.NewToDevice(sh.Storage))
	sh.streams = append(sh.streams, streams.NewRoomMember(sh.Storage))

	latestToken := sync3.NewBlankSyncToken(0, 0)
	nid, err := sh.Storage.LatestEventNID()
	if err != nil {
		panic(err)
	}
	latestToken.SetEventPosition(nid)
	typingPos, err := sh.Storage.LatestTypingID()
	if err != nil {
		panic(err)
	}
	latestToken.SetTypingPosition(typingPos)
	sh.Notifier = notifier.NewNotifier(*latestToken)
	roomIDToUserIDs, err := sh.Storage.AllJoinedMembers()
	if err != nil {
		panic(err)
	}
	sh.Notifier.Load(roomIDToUserIDs)
	return sh
}

func (h *SyncV3Handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	err := h.serve(w, req)
	if err != nil {
		w.WriteHeader(err.StatusCode)
		w.Write(err.JSON())
	}
}

func (h *SyncV3Handler) serve(w http.ResponseWriter, req *http.Request) *internal.HandlerError {
	session, fromToken, herr := h.getOrCreateSession(req)
	if herr != nil {
		return herr
	}
	log := hlog.FromRequest(req).With().Int64("session", session.ID).Logger()
	log.Info().Str("device", session.V2.DeviceID).Str("user_id", session.V2.UserID).Str("since", req.URL.Query().Get("since")).Str("from", fromToken.String()).Msg("recv /v3/sync")

	// make sure we have a poller for this device
	h.PollerMap.EnsurePolling(
		req.Header.Get("Authorization"), session.V2.UserID, session.V2.DeviceID, session.V2.Since, log,
	)

	// fetch the latest value which we'll base our response on
	upcoming := h.Notifier.CurrentPosition()
	upcoming.AssociateWithUser(*fromToken)
	timeout := req.URL.Query().Get("timeout")
	if timeout == "" {
		timeout = "0"
	}
	timeoutMs, err := strconv.ParseInt(timeout, 10, 64)
	if err != nil {
		return &internal.HandlerError{
			StatusCode: 400,
			Err:        fmt.Errorf("?timeout= isn't an integer"),
		}
	}

	// Track confirmed/unconfirmed tokens. We do this in a defer because we can bail either with
	// or without data, so deferring it keeps it in one place.
	// This will invoke streams with the confirmed position based on ?since= for this session
	// for the purposes of cleaning up old messages which all sessions have confirmed.
	defer func() {
		// finally update our records: confirm that the client received the token they sent us, and mark this
		// response as unconfirmed
		if err := h.Sessions.UpdateLastTokens(session.ID, fromToken.String(), upcoming.String()); err != nil {
			log.Err(err).Msg("Failed to update last sent tokens")
		}
		h.confirmPositionCleanup(session, fromToken)
	}()

	// read filters and mux in to form complete request. We MUST do this before waiting on sync streams
	// so that we update filters even in the event that we time out
	syncReq, filterID, herr := h.parseRequest(req, fromToken, session)
	if herr != nil {
		return herr
	}
	// if there was a change to the filters, update the filter ID
	if filterID != 0 {
		upcoming.FilterID = filterID
	}

	if !shouldReturnImmediately(syncReq, h.streams, fromToken, &upcoming, timeoutMs) {
		// from == upcoming so we need to block for up to timeoutMs for a new event to come in
		log.Info().Int64("timeout_ms", timeoutMs).Msg("blocking")
		newUpcoming := h.waitForEvents(req.Context(), session, *fromToken, time.Duration(timeoutMs)*time.Millisecond)
		if newUpcoming == nil {
			// no data
			w.WriteHeader(200)
			var result streams.Response
			result.Next = upcoming.String()
			if err := json.NewEncoder(w).Encode(&result); err != nil {
				log.Warn().Err(err).Msg("failed to marshal response")
			}
			return nil
		}
		upcoming.ApplyUpdates(*newUpcoming)
	}

	// start making the response
	resp := streams.Response{
		Events: make(map[string]json.RawMessage),
	}

	// invoke streams to get responses
	for _, stream := range h.streams {
		fromExcl := stream.Position(fromToken)
		toIncl := stream.Position(&upcoming)
		upTo, err := stream.DataInRange(session, fromExcl, toIncl, syncReq, &resp)
		if err == streams.ErrNotRequested {
			continue
		}
		if err != nil {
			return &internal.HandlerError{
				StatusCode: 500,
				Err:        fmt.Errorf("stream error: %s", err),
			}
		}
		if upTo != 0 {
			// update the to position for this stream. Calling DataInRange is allowed to modify the to
			// value in cases where a LIMIT gets applied
			stream.SetPosition(&upcoming, upTo)
		}
	}

	resp.Next = upcoming.String()
	log.Info().Str("since", fromToken.String()).Str("new_since", upcoming.String()).Bools(
		"request[typing,to_device,room_member]",
		[]bool{syncReq.Typing != nil, syncReq.ToDevice != nil, syncReq.RoomMember != nil},
	).Msg("responding")

	w.WriteHeader(200)
	if err := json.NewEncoder(w).Encode(&resp); err != nil {
		log.Warn().Err(err).Msg("failed to marshal response")
	}

	return nil
}

// Loads all the tokens that have been confirmed by all sessions on this device ID. This means we know
// that the clients have processed up to and including the confirmed token. Therefore, it is safe to
// cleanup older data if there are no more sessions on earlier positions.
func (h *SyncV3Handler) confirmPositionCleanup(session *sync3.Session, from *sync3.Token) {
	// Load the tokens which have been sent by every session on this device.
	// We'll use this to work out the minimum position confirmed and then invoke the cleanup
	confirmedTokenStrings, err := h.Sessions.ConfirmedSessionTokens(session.V2.DeviceID)
	if err != nil {
		log.Err(err).Msg("failed to get ConfirmedSessionTokens") // non-fatal
		return
	}
	confirmedTokens := []*sync3.Token{from}
	for _, tokStr := range confirmedTokenStrings {
		tok, err := sync3.NewSyncToken(tokStr)
		if err != nil {
			log.Err(err).Msg("failed to load confirmed token from ConfirmedSessionTokens")
		} else {
			confirmedTokens = append(confirmedTokens, tok)
		}
	}
	for _, stream := range h.streams {
		confirmedPos := stream.Position(from)
		// check if all session are up-to or past this point
		allSessions := true
		for _, tok := range confirmedTokens {
			pos := stream.Position(tok)
			if pos < confirmedPos {
				allSessions = false
				break
			}
		}

		stream.SessionConfirmed(session, confirmedPos, allSessions)
	}
}

// getOrCreateSession retrieves an existing session if ?since= is set, else makes a new session.
// Returns a session or an error.
func (h *SyncV3Handler) getOrCreateSession(req *http.Request) (*sync3.Session, *sync3.Token, *internal.HandlerError) {
	log := hlog.FromRequest(req)
	var session *sync3.Session
	var tokv3 *sync3.Token
	deviceID, err := internal.DeviceIDFromRequest(req)
	if err != nil {
		log.Warn().Err(err).Msg("failed to get device ID from request")
		return nil, nil, &internal.HandlerError{
			StatusCode: 400,
			Err:        err,
		}
	}
	sincev3 := req.URL.Query().Get("since")
	if sincev3 == "" {
		session, err = h.Sessions.NewSession(deviceID)
	} else {
		tokv3, err = sync3.NewSyncToken(sincev3)
		if err != nil {
			log.Warn().Err(err).Msg("failed to parse sync v3 token")
			return nil, nil, &internal.HandlerError{
				StatusCode: 400,
				Err:        err,
			}
		}
		session, err = h.Sessions.Session(tokv3.SessionID, deviceID)
	}
	if err != nil {
		log.Warn().Err(err).Str("device", deviceID).Msg("failed to ensure Session existed for device")
		return nil, nil, &internal.HandlerError{
			StatusCode: 500,
			Err:        err,
		}
	}
	if session == nil {
		return nil, nil, &internal.HandlerError{
			StatusCode: 400,
			Err:        fmt.Errorf("unknown session; since = %s session ID = %d device ID = %s", sincev3, tokv3.SessionID, deviceID),
		}
	}
	if session.V2.UserID == "" {
		// we need to work out the user ID to do membership queries
		userID, err := h.userIDFromRequest(req)
		if err != nil {
			log.Warn().Err(err).Msg("failed to work out user ID from request, is the authorization header valid?")
			return nil, nil, &internal.HandlerError{
				StatusCode: 400,
				Err:        err,
			}
		}
		session.V2.UserID = userID
		h.Sessions.UpdateUserIDForDevice(deviceID, userID)
	}
	if tokv3 == nil {
		tokv3 = sync3.NewBlankSyncToken(session.ID, 0)
	}
	return session, tokv3, nil
}

// Called from the v2 poller, implements V2DataReceiver
func (h *SyncV3Handler) UpdateDeviceSince(deviceID, since string) error {
	return h.Sessions.UpdateDeviceSince(deviceID, since)
}

// Called from the v2 poller, implements V2DataReceiver
func (h *SyncV3Handler) Accumulate(roomID string, timeline []json.RawMessage) error {
	numNew, nid, err := h.Storage.Accumulate(roomID, timeline)
	if err != nil {
		return err
	}
	if numNew == 0 {
		return nil
	}
	updateToken := sync3.NewBlankSyncToken(0, 0)
	updateToken.SetEventPosition(nid)
	newEvents := timeline[len(timeline)-numNew:]
	for _, eventJSON := range newEvents {
		event := gjson.ParseBytes(eventJSON)
		h.Notifier.OnNewEvent(
			roomID, event.Get("sender").Str, event.Get("type").Str,
			event.Get("state_key").Str, event.Get("content.membership").Str, nil, *updateToken,
		)
	}
	return nil
}

// Called from the v2 poller, implements V2DataReceiver
func (h *SyncV3Handler) Initialise(roomID string, state []json.RawMessage) error {
	added, err := h.Storage.Initialise(roomID, state)
	if err != nil {
		return err
	}
	if added {
		for _, eventJSON := range state {
			event := gjson.ParseBytes(eventJSON)
			if event.Get("type").Str == "m.room.member" {
				target := event.Get("state_key").Str
				membership := event.Get("content.membership").Str
				if membership == "join" {
					h.Notifier.AddJoinedUser(roomID, target)
				} else if membership == "ban" || membership == "leave" {
					h.Notifier.RemoveJoinedUser(roomID, target)
				}
			}
		}
	}
	return nil
}

// Called from the v2 poller, implements V2DataReceiver
func (h *SyncV3Handler) SetTyping(roomID string, userIDs []string) (int64, error) {
	pos, err := h.Storage.TypingTable.SetTyping(roomID, userIDs)
	if err != nil {
		return 0, err
	}
	updateToken := sync3.NewBlankSyncToken(0, 0)
	updateToken.SetTypingPosition(pos)
	h.Notifier.OnNewTyping(roomID, *updateToken)
	return pos, nil
}

func (h *SyncV3Handler) AddToDeviceMessages(userID, deviceID string, msgs []gomatrixserverlib.SendToDeviceEvent) error {
	pos, err := h.Storage.ToDeviceTable.InsertMessages(deviceID, msgs)
	if err != nil {
		return err
	}
	updateToken := sync3.NewBlankSyncToken(0, 0)
	updateToken.SetToDevicePosition(pos)
	h.Notifier.OnNewSendToDevice(userID, []string{deviceID}, *updateToken)
	return nil
}

func (h *SyncV3Handler) parseRequest(req *http.Request, tok *sync3.Token, session *sync3.Session) (*streams.Request, int64, *internal.HandlerError) {
	existing := &streams.Request{} // first request
	var err error
	if tok.FilterID != 0 {
		// load existing filter
		existing, err = h.Sessions.Filter(tok.SessionID, tok.FilterID)
		if err != nil {
			return nil, 0, &internal.HandlerError{
				StatusCode: 400,
				Err:        fmt.Errorf("failed to load filters from sync token: %s", err),
			}
		}
	}
	// load new delta from request
	defer req.Body.Close()
	var delta streams.Request
	if err := json.NewDecoder(req.Body).Decode(&delta); err != nil {
		return nil, 0, &internal.HandlerError{
			StatusCode: 400,
			Err:        fmt.Errorf("failed to parse request body as JSON: %s", err),
		}
	}
	var filterID int64
	deltasExist, err := existing.ApplyDeltas(&delta)
	if err != nil {
		return nil, 0, &internal.HandlerError{
			StatusCode: 400,
			Err:        fmt.Errorf("failed to parse request body delta as JSON: %s", err),
		}
	}
	if deltasExist {
		// persist new filters if there were deltas
		filterID, err = h.Sessions.InsertFilter(session.ID, existing)
		if err != nil {
			return nil, 0, &internal.HandlerError{
				StatusCode: 500,
				Err:        fmt.Errorf("failed to persist filters: %s", err),
			}
		}
	}

	return existing, filterID, nil
}

func (h *SyncV3Handler) userIDFromRequest(req *http.Request) (string, error) {
	return h.V2.WhoAmI(req.Header.Get("Authorization"))
}

func (h *SyncV3Handler) waitForEvents(ctx context.Context, session *sync3.Session, since sync3.Token, timeout time.Duration) *sync3.Token {
	listener := h.Notifier.GetListener(ctx, *session)
	defer listener.Close()
	select {
	case <-ctx.Done():
		// caller gave up
		return nil
	case <-time.After(timeout):
		// timed out
		return nil
	case <-listener.GetNotifyChannel(since):
		// new data!
		p := listener.GetSyncPosition()
		return &p
	}
}

func shouldReturnImmediately(req *streams.Request, streams []streams.Streamer, fromToken, upcoming *sync3.Token, timeoutMs int64) bool {
	if timeoutMs == 0 {
		return true
	}
	newerTokenExists := upcoming.IsAfter(*fromToken)
	if newerTokenExists {
		return true
	}
	// check if there is a pagination request here
	for _, s := range streams {
		if s.IsPaginationRequest(req) {
			return true
		}
	}
	return false
}
