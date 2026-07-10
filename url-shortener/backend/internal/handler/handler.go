// Package handler adapts the generated API strict server interface to the
// service layer. It only translates between generated types and domain
// calls/errors; all business logic lives in internal/service.
package handler

import (
	"context"
	"errors"
	"strings"

	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/api"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/service"
)

// Handler implements api.StrictServerInterface.
type Handler struct {
	svc     *service.LinkService
	baseURL string
}

var _ api.StrictServerInterface = (*Handler)(nil)

// New returns a Handler. baseURL is the public origin used to build short URLs,
// e.g. "http://localhost:8080" (no trailing slash required).
func New(svc *service.LinkService, baseURL string) *Handler {
	return &Handler{svc: svc, baseURL: strings.TrimRight(baseURL, "/")}
}

// CreateLink handles POST /links.
func (h *Handler) CreateLink(ctx context.Context, request api.CreateLinkRequestObject) (api.CreateLinkResponseObject, error) {
	if request.Body == nil {
		return api.CreateLink400JSONResponse(apiError("request body is required")), nil
	}
	link, err := h.svc.Create(ctx, request.Body.Url, request.Body.CustomAlias)
	switch {
	case errors.Is(err, service.ErrInvalidURL):
		return api.CreateLink400JSONResponse(apiError("url must be a valid http(s) URL")), nil
	case errors.Is(err, service.ErrInvalidAlias):
		return api.CreateLink400JSONResponse(apiError("custom_alias must match ^[A-Za-z0-9_-]{3,32}$")), nil
	case errors.Is(err, service.ErrAliasTaken):
		return api.CreateLink409JSONResponse(apiError("custom_alias is already in use")), nil
	case err != nil:
		return nil, err
	}
	return api.CreateLink201JSONResponse(h.toAPI(link)), nil
}

// GetLink handles GET /links/{code}.
func (h *Handler) GetLink(ctx context.Context, request api.GetLinkRequestObject) (api.GetLinkResponseObject, error) {
	link, err := h.svc.Get(ctx, request.Code)
	if errors.Is(err, service.ErrNotFound) {
		return api.GetLink404JSONResponse(apiError("link not found")), nil
	}
	if err != nil {
		return nil, err
	}
	return api.GetLink200JSONResponse(h.toAPI(link)), nil
}

// ListLinks handles GET /links.
func (h *Handler) ListLinks(ctx context.Context, request api.ListLinksRequestObject) (api.ListLinksResponseObject, error) {
	limit := 20
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	offset := 0
	if request.Params.Offset != nil {
		offset = *request.Params.Offset
	}

	links, total, err := h.svc.List(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	items := make([]api.Link, len(links))
	for i, l := range links {
		items[i] = h.toAPI(l)
	}
	return api.ListLinks200JSONResponse{Items: items, Total: total}, nil
}

// DeleteLink handles DELETE /links/{code}.
func (h *Handler) DeleteLink(ctx context.Context, request api.DeleteLinkRequestObject) (api.DeleteLinkResponseObject, error) {
	err := h.svc.Delete(ctx, request.Code)
	if errors.Is(err, service.ErrNotFound) {
		return api.DeleteLink404JSONResponse(apiError("link not found")), nil
	}
	if err != nil {
		return nil, err
	}
	return api.DeleteLink204Response{}, nil
}

// toAPI converts a domain link into the API representation.
func (h *Handler) toAPI(l repository.Link) api.Link {
	return api.Link{
		Code:       l.Code,
		Url:        l.URL,
		ShortUrl:   h.baseURL + "/" + l.Code,
		VisitCount: l.VisitCount,
		CreatedAt:  l.CreatedAt,
	}
}

func apiError(msg string) api.Error {
	return api.Error{Message: msg}
}
