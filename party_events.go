package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gather-your-party/internal/db"
	"gather-your-party/internal/middleware"
	"gather-your-party/internal/view"
)

// sharedGamesUnavailable is shown when the party's shared games cannot be built.
const sharedGamesUnavailable = "We could not load shared games right now. Please try again in a moment."

// parseUTCOffset parses a browser-supplied UTC offset in the form "+HH:MM" or
// "-HH:MM" (east of UTC positive, as the party page script writes it) into
// seconds east of UTC.
func parseUTCOffset(s string) (int, error) {
	if len(s) != 6 || (s[0] != '+' && s[0] != '-') || s[3] != ':' {
		return 0, errors.New("offset must look like +HH:MM")
	}
	hours, err := strconv.Atoi(s[1:3])
	if err != nil || hours > 14 {
		return 0, errors.New("offset hours out of range")
	}
	minutes, err := strconv.Atoi(s[4:6])
	if err != nil || minutes > 59 {
		return 0, errors.New("offset minutes out of range")
	}
	seconds := hours*3600 + minutes*60
	if s[0] == '-' {
		seconds = -seconds
	}
	return seconds, nil
}

// eventInstants turns the submitted local date, start and end times and UTC
// offset into absolute instants. It returns a message naming the problem when
// a field is missing or unparseable, or when the end is not after the start.
func eventInstants(date, startTime, endTime, offset string) (time.Time, time.Time, string) {
	if strings.TrimSpace(date) == "" {
		return time.Time{}, time.Time{}, "date is required"
	}
	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}, time.Time{}, "date must be a valid YYYY-MM-DD date"
	}
	if strings.TrimSpace(startTime) == "" {
		return time.Time{}, time.Time{}, "start time is required"
	}
	start, err := time.Parse("15:04", startTime)
	if err != nil {
		return time.Time{}, time.Time{}, "start time must be a valid HH:MM time"
	}
	if strings.TrimSpace(endTime) == "" {
		return time.Time{}, time.Time{}, "end time is required"
	}
	end, err := time.Parse("15:04", endTime)
	if err != nil {
		return time.Time{}, time.Time{}, "end time must be a valid HH:MM time"
	}
	if strings.TrimSpace(offset) == "" {
		return time.Time{}, time.Time{}, "UTC offset is required"
	}
	seconds, err := parseUTCOffset(offset)
	if err != nil {
		return time.Time{}, time.Time{}, "UTC offset must look like +HH:MM or -HH:MM"
	}
	zone := time.FixedZone(offset, seconds)
	startAt := time.Date(day.Year(), day.Month(), day.Day(), start.Hour(), start.Minute(), 0, 0, zone)
	endAt := time.Date(day.Year(), day.Month(), day.Day(), end.Hour(), end.Minute(), 0, 0, zone)
	if !endAt.After(startAt) {
		return time.Time{}, time.Time{}, "end must be after the start"
	}
	return startAt.UTC(), endAt.UTC(), ""
}

// handleCreatePartyEvent lets the party leader schedule an event for one of the
// party's shared games. A non-member gets 403; any other member is sent back to
// the party page; nothing is saved unless every field is valid and the game is
// in the party's current shared games. Any client-supplied game name is ignored.
func (app *application) handleCreatePartyEvent(ctx *middleware.CustomContext, w http.ResponseWriter, r *http.Request) {
	partyID, userID, ok := app.leaveIdentity(ctx, w, r)
	if !ok {
		return
	}
	member, err := app.store.IsMember(r.Context(), partyID, userID)
	if err != nil {
		http.Error(w, "unable to check membership", http.StatusInternalServerError)
		return
	}
	if !member {
		http.Error(w, "party members only", http.StatusForbidden)
		return
	}
	partyPage := "/parties/" + partyID
	members, err := app.store.PartyMembers(r.Context(), partyID)
	if err != nil {
		http.Error(w, "unable to load party members", http.StatusInternalServerError)
		return
	}
	isLeader := false
	for _, m := range members {
		if m.UserID == userID && m.IsLeader {
			isLeader = true
		}
	}
	if !isLeader {
		http.Redirect(w, r, partyPage, http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	startAt, endAt, problem := eventInstants(r.PostForm.Get("date"), r.PostForm.Get("start_time"), r.PostForm.Get("end_time"), r.PostForm.Get("utc_offset"))
	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	appID, err := strconv.Atoi(strings.TrimSpace(r.PostForm.Get("game_app_id")))
	if err != nil || appID <= 0 {
		http.Error(w, "game must be one of the party's shared games", http.StatusBadRequest)
		return
	}
	gamesCtx, cancelGames := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancelGames()
	sharedGames, _, err := view.BuildSharedGames(gamesCtx, app.steamService()(), app.store, members)
	if err != nil {
		fmt.Printf("create party event: shared games error: %s\n", err)
		http.Error(w, sharedGamesUnavailable, http.StatusServiceUnavailable)
		return
	}
	shared := false
	for _, game := range sharedGames {
		if game.Game.AppID == appID {
			shared = true
			break
		}
	}
	if !shared {
		http.Error(w, "game is not in the party's shared games", http.StatusBadRequest)
		return
	}
	if err := app.store.CreatePartyEvent(r.Context(), partyID, appID, startAt, endAt); err != nil {
		if errors.Is(err, db.ErrUnknownGame) {
			http.Error(w, "game is not in the party's shared games", http.StatusBadRequest)
			return
		}
		fmt.Printf("create party event: %s\n", err)
		http.Error(w, "unable to save the event", http.StatusInternalServerError)
		return
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, partyPage, http.StatusSeeOther)
}
