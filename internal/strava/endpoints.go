package strava

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Athlete is the shape returned by /athlete and /athletes/:id/{followers,friends}.
// Only fields the bot uses are modeled.
type Athlete struct {
	ID        int64  `json:"id"`
	Firstname string `json:"firstname"`
	Lastname  string `json:"lastname"`
}

// DisplayName is "Firstname Lastname".
func (a Athlete) DisplayName() string { return a.Firstname + " " + a.Lastname }

// FeedItem models a single card in /feed/athlete/:id. Strava's modular feed
// is variable-shaped; we extract just what we need from the two common forms.
type FeedItem struct {
	ActivityID int64
	HasKudoed  bool
}

// AuthResponse is the relevant subset of the /oauth/internal/token response.
type AuthResponse struct {
	AccessToken string `json:"access_token"`
}

// GetProfile returns the authenticated athlete's id.
func (c *Client) GetProfile(ctx context.Context, token string) (int64, error) {
	resp, err := c.do(ctx, "GET", "/api/v3/athlete?hl=en", nil, token)
	if err != nil {
		return 0, err
	}
	var a Athlete
	if err := json.Unmarshal(resp.Body, &a); err != nil {
		return 0, fmt.Errorf("GetProfile: %w", err)
	}
	return a.ID, nil
}

// GetFollowers returns the list of athletes following `athleteID`.
// Single-page only — Strava returns enough for a personal account.
func (c *Client) GetFollowers(ctx context.Context, token string, athleteID int64) ([]Athlete, error) {
	return c.getAthleteList(ctx, token, "followers", athleteID)
}

// GetFriends returns the list of athletes `athleteID` is following.
// (Strava uses the legacy term "friends" for this; `/following` returns 404.)
func (c *Client) GetFriends(ctx context.Context, token string, athleteID int64) ([]Athlete, error) {
	return c.getAthleteList(ctx, token, "friends", athleteID)
}

func (c *Client) getAthleteList(ctx context.Context, token, kind string, athleteID int64) ([]Athlete, error) {
	path := fmt.Sprintf("/api/v3/athletes/%d/%s?hl=en", athleteID, kind)
	resp, err := c.do(ctx, "GET", path, nil, token)
	if err != nil {
		return nil, err
	}
	var out []Athlete
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("get %s: %w", kind, err)
	}
	return out, nil
}

// feedRawItem matches the outer `{ entity_id, item: {...} }` shape used by
// Strava's modular feed. item.has_kudoed is the boolean we care about.
type feedRawItem struct {
	EntityID int64           `json:"entity_id"`
	Item     feedRawItemBody `json:"item"`
}

type feedRawItemBody struct {
	HasKudoed *bool `json:"has_kudoed"`
}

// GetFeed returns kudosable activities from an athlete's feed.
// Items without an entity_id or without has_kudoed are skipped — those are
// non-activity cards (RouteSuggested, SegmentLeaderboard, etc.).
func (c *Client) GetFeed(ctx context.Context, token string, athleteID int64) ([]FeedItem, error) {
	path := fmt.Sprintf(
		"/api/v3/feed/athlete/%d?photo_sizes[]=240&single_entity_supported=true&modular=true&hl=en",
		athleteID,
	)
	resp, err := c.do(ctx, "GET", path, nil, token)
	if err != nil {
		return nil, err
	}
	var raw []feedRawItem
	if err := json.Unmarshal(resp.Body, &raw); err != nil {
		return nil, fmt.Errorf("GetFeed: %w", err)
	}
	out := make([]FeedItem, 0, len(raw))
	for _, r := range raw {
		if r.EntityID == 0 || r.Item.HasKudoed == nil {
			continue
		}
		out = append(out, FeedItem{
			ActivityID: r.EntityID,
			HasKudoed:  *r.Item.HasKudoed,
		})
	}
	return out, nil
}

// PostKudos posts a kudos on activityID. Returns the HTTP status code — the
// caller writes it to kudos_log.api_status verbatim for audit.
//
// Idempotency: 201 = newly created, 200 = already kudoed (treated OK),
// 409 = conflict / already kudoed (also treated OK).
func (c *Client) PostKudos(ctx context.Context, token string, activityID int64) (int, error) {
	path := fmt.Sprintf("/api/v3/activities/%d/kudos?hl=en", activityID)
	resp, err := c.do(ctx, "POST", path, bytes.NewReader(nil), token)
	if resp == nil && err != nil {
		// network-level failure — no status
		return 0, err
	}
	status := resp.StatusCode
	switch status {
	case http.StatusCreated, http.StatusOK:
		return status, nil
	case http.StatusConflict:
		// Strava sometimes returns 409 for "already kudoed". Idempotent success.
		return status, nil
	}
	// For other statuses do() already produced the right sentinel error.
	return status, err
}

// Auth performs the password-grant login against the mobile API.
// client_id=2 is hardcoded — it's the official mobile client ID extracted from
// the Android app. Do not change.
func (c *Client) Auth(ctx context.Context, clientSecret, email, password string) (AuthResponse, error) {
	payload := map[string]any{
		"client_id":     2,
		"client_secret": clientSecret,
		"email":         email,
		"password":      password,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return AuthResponse{}, fmt.Errorf("Auth marshal: %w", err)
	}
	resp, err := c.do(ctx, "POST", "/api/v3/oauth/internal/token?hl=en", bytes.NewReader(body), "")
	if err != nil {
		return AuthResponse{}, err
	}
	var a AuthResponse
	if err := json.Unmarshal(resp.Body, &a); err != nil {
		return AuthResponse{}, fmt.Errorf("Auth decode: %w", err)
	}
	if a.AccessToken == "" {
		return AuthResponse{}, errors.New("Auth: empty access_token in response")
	}
	return a, nil
}
