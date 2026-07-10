// Package handler adapts the generated strict-server interface to the service
// layer. It only translates between generated types and domain calls/errors
// (including string<->int64 id conversion); all logic lives in internal/service.
package handler

import (
	"context"
	"errors"
	"strconv"

	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/api"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/twitter/backend/internal/service"
)

// defaultLimit mirrors the openapi limit default.
const defaultLimit = 20

// Handler implements api.StrictServerInterface.
type Handler struct {
	svc *service.Service
}

var _ api.StrictServerInterface = (*Handler)(nil)

// New returns a Handler backed by svc.
func New(svc *service.Service) *Handler {
	return &Handler{svc: svc}
}

// CreateUser handles POST /users.
func (h *Handler) CreateUser(ctx context.Context, request api.CreateUserRequestObject) (api.CreateUserResponseObject, error) {
	if request.Body == nil {
		return api.CreateUser400JSONResponse(apiError("request body is required")), nil
	}
	var displayName string
	if request.Body.DisplayName != nil {
		displayName = *request.Body.DisplayName
	}
	u, err := h.svc.CreateUser(ctx, request.Body.Handle, displayName)
	switch {
	case errors.Is(err, service.ErrInvalidHandle):
		return api.CreateUser400JSONResponse(apiError("handle must match ^[A-Za-z0-9_]{1,15}$")), nil
	case errors.Is(err, service.ErrHandleTaken):
		return api.CreateUser409JSONResponse(apiError("handle is already in use")), nil
	case err != nil:
		return nil, err
	}
	return api.CreateUser201JSONResponse(toAPIUser(u)), nil
}

// GetUser handles GET /users/{id}.
func (h *Handler) GetUser(ctx context.Context, request api.GetUserRequestObject) (api.GetUserResponseObject, error) {
	id, ok := parseID(request.Id)
	if !ok {
		return api.GetUser404JSONResponse(apiError("user not found")), nil
	}
	u, err := h.svc.GetUser(ctx, id)
	if errors.Is(err, service.ErrNotFound) {
		return api.GetUser404JSONResponse(apiError("user not found")), nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetUser200JSONResponse(toAPIUser(u)), nil
}

// Follow handles PUT /users/{id}/following/{targetId}.
func (h *Handler) Follow(ctx context.Context, request api.FollowRequestObject) (api.FollowResponseObject, error) {
	followerID, ok1 := parseID(request.Id)
	followeeID, ok2 := parseID(request.TargetId)
	if !ok1 || !ok2 {
		return api.Follow400JSONResponse(apiError("ids must be integers")), nil
	}
	err := h.svc.Follow(ctx, followerID, followeeID)
	switch {
	case errors.Is(err, service.ErrSelfFollow):
		return api.Follow400JSONResponse(apiError("cannot follow yourself")), nil
	case errors.Is(err, service.ErrNotFound):
		return api.Follow404JSONResponse(apiError("user not found")), nil
	case err != nil:
		return nil, err
	}
	return api.Follow204Response{}, nil
}

// Unfollow handles DELETE /users/{id}/following/{targetId}.
func (h *Handler) Unfollow(ctx context.Context, request api.UnfollowRequestObject) (api.UnfollowResponseObject, error) {
	followerID, ok1 := parseID(request.Id)
	followeeID, ok2 := parseID(request.TargetId)
	if !ok1 || !ok2 {
		return api.Unfollow404JSONResponse(apiError("user not found")), nil
	}
	err := h.svc.Unfollow(ctx, followerID, followeeID)
	if errors.Is(err, service.ErrNotFound) {
		return api.Unfollow404JSONResponse(apiError("user not found")), nil
	}
	if err != nil {
		return nil, err
	}
	return api.Unfollow204Response{}, nil
}

// CreateTweet handles POST /tweets.
func (h *Handler) CreateTweet(ctx context.Context, request api.CreateTweetRequestObject) (api.CreateTweetResponseObject, error) {
	if request.Body == nil {
		return api.CreateTweet400JSONResponse(apiError("request body is required")), nil
	}
	authorID, ok := parseID(request.Body.AuthorId)
	if !ok {
		return api.CreateTweet400JSONResponse(apiError("author_id must be an integer")), nil
	}
	t, err := h.svc.PostTweet(ctx, authorID, request.Body.Text)
	switch {
	case errors.Is(err, service.ErrInvalidText):
		return api.CreateTweet400JSONResponse(apiError("text must be 1..280 characters")), nil
	case errors.Is(err, service.ErrNotFound):
		return api.CreateTweet404JSONResponse(apiError("author not found")), nil
	case err != nil:
		return nil, err
	}
	return api.CreateTweet201JSONResponse(toAPITweet(t)), nil
}

// ListUserTweets handles GET /users/{id}/tweets.
func (h *Handler) ListUserTweets(ctx context.Context, request api.ListUserTweetsRequestObject) (api.ListUserTweetsResponseObject, error) {
	id, ok := parseID(request.Id)
	if !ok {
		return api.ListUserTweets404JSONResponse(apiError("user not found")), nil
	}
	before := parseCursor(request.Params.BeforeId)
	limit := limitOrDefault(request.Params.Limit)

	tweets, next, err := h.svc.UserTimeline(ctx, id, before, limit)
	if errors.Is(err, service.ErrNotFound) {
		return api.ListUserTweets404JSONResponse(apiError("user not found")), nil
	}
	if err != nil {
		return nil, err
	}
	return api.ListUserTweets200JSONResponse(toAPITweetList(tweets, next)), nil
}

// GetHomeTimeline handles GET /timelines/home.
func (h *Handler) GetHomeTimeline(ctx context.Context, request api.GetHomeTimelineRequestObject) (api.GetHomeTimelineResponseObject, error) {
	id, ok := parseID(request.Params.UserId)
	if !ok {
		return api.GetHomeTimeline404JSONResponse(apiError("user not found")), nil
	}
	before := parseCursor(request.Params.BeforeId)
	limit := limitOrDefault(request.Params.Limit)

	tweets, next, err := h.svc.HomeTimeline(ctx, id, before, limit)
	if errors.Is(err, service.ErrNotFound) {
		return api.GetHomeTimeline404JSONResponse(apiError("user not found")), nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetHomeTimeline200JSONResponse(toAPITweetList(tweets, next)), nil
}

// --- conversions ---------------------------------------------------------

func toAPIUser(u repository.User) api.User {
	return api.User{
		Id:            formatID(u.ID),
		Handle:        u.Handle,
		DisplayName:   u.DisplayName,
		FollowerCount: u.FollowerCount,
		CreatedAt:     u.CreatedAt,
	}
}

func toAPITweet(t repository.Tweet) api.Tweet {
	return api.Tweet{
		Id:           formatID(t.ID),
		AuthorId:     formatID(t.AuthorID),
		AuthorHandle: t.AuthorHandle,
		Text:         t.Text,
		CreatedAt:    t.CreatedAt,
	}
}

func toAPITweetList(tweets []repository.Tweet, next int64) api.TweetList {
	items := make([]api.Tweet, len(tweets))
	for i, t := range tweets {
		items[i] = toAPITweet(t)
	}
	list := api.TweetList{Items: items}
	if next > 0 {
		cursor := formatID(next)
		list.NextCursor = &cursor
	}
	return list
}

func formatID(id int64) string { return strconv.FormatInt(id, 10) }

// parseID parses a decimal id; ok is false for a malformed or non-positive id.
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// parseCursor parses an optional before_id; a missing or malformed cursor means
// "from newest" (0).
func parseCursor(s *string) int64 {
	if s == nil {
		return 0
	}
	id, err := strconv.ParseInt(*s, 10, 64)
	if err != nil || id < 0 {
		return 0
	}
	return id
}

func limitOrDefault(limit *int) int {
	if limit == nil {
		return defaultLimit
	}
	return *limit
}

func apiError(msg string) api.Error {
	return api.Error{Message: msg}
}
