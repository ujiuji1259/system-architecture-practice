// Package service holds the URL-shortening domain logic, independent of the
// HTTP/transport layer and of the generated API types.
package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/repository"
	"github.com/ujiuji1259/system-architecture-practice/url-shortener/backend/internal/shortcode"
)

// Domain errors. The transport layer maps these to status codes.
var (
	// ErrInvalidURL is returned when the target URL is not a valid http(s) URL.
	ErrInvalidURL = errors.New("invalid url")
	// ErrInvalidAlias is returned when a custom alias violates the format.
	ErrInvalidAlias = errors.New("invalid custom alias")
	// ErrAliasTaken is returned when a custom alias is already in use.
	ErrAliasTaken = errors.New("custom alias already in use")
	// ErrNotFound is returned when a link does not exist.
	ErrNotFound = errors.New("link not found")
)

// aliasPattern mirrors the custom_alias schema in openapi.yaml.
var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{3,32}$`)

// maxGenAttempts bounds retries when a generated code collides.
const maxGenAttempts = 5

// Repository is the persistence behavior the service depends on. How the visit
// count is stored or cached is entirely the repository's concern; the service
// just asks for a link and gets its count.
type Repository interface {
	Create(ctx context.Context, l repository.Link) error
	Get(ctx context.Context, code string) (repository.Link, error)
	List(ctx context.Context, limit, offset int) ([]repository.Link, int64, error)
	Delete(ctx context.Context, code string) error
	RecordVisit(ctx context.Context, code string, ev repository.Event) (string, error)
	ListEvents(ctx context.Context, code string, limit, offset int) ([]repository.Event, int64, error)
}

// LinkService implements the shortening use cases over a Repository.
type LinkService struct {
	repo Repository
}

// New returns a LinkService backed by repo.
func New(repo Repository) *LinkService {
	return &LinkService{repo: repo}
}

// Create validates the URL and either honors the custom alias or generates a
// unique short code, then persists the link.
func (s *LinkService) Create(ctx context.Context, rawURL string, customAlias *string) (repository.Link, error) {
	rawURL, err := validateURL(rawURL)
	if err != nil {
		return repository.Link{}, err
	}

	// Custom alias: honor exactly what the caller asked for.
	if customAlias != nil {
		if err := validateAlias(*customAlias); err != nil {
			return repository.Link{}, err
		}
		link := repository.Link{Code: *customAlias, URL: rawURL, CreatedAt: time.Now()}
		switch err := s.repo.Create(ctx, link); {
		case errors.Is(err, repository.ErrCodeExists):
			return repository.Link{}, ErrAliasTaken
		case err != nil:
			return repository.Link{}, err
		}
		return link, nil
	}

	// Generated code: retry on the rare collision.
	for attempt := 0; attempt < maxGenAttempts; attempt++ {
		code, err := shortcode.Generate(shortcode.DefaultLength)
		if err != nil {
			return repository.Link{}, err
		}
		link := repository.Link{Code: code, URL: rawURL, CreatedAt: time.Now()}
		switch err := s.repo.Create(ctx, link); {
		case errors.Is(err, repository.ErrCodeExists):
			continue
		case err != nil:
			return repository.Link{}, err
		}
		return link, nil
	}
	return repository.Link{}, fmt.Errorf("failed to generate a unique code after %d attempts", maxGenAttempts)
}

// Get returns the link for a code, or ErrNotFound.
func (s *LinkService) Get(ctx context.Context, code string) (repository.Link, error) {
	link, err := s.repo.Get(ctx, code)
	if errors.Is(err, repository.ErrNotFound) {
		return repository.Link{}, ErrNotFound
	}
	return link, err
}

// List returns a page of links plus the total count.
func (s *LinkService) List(ctx context.Context, limit, offset int) ([]repository.Link, int64, error) {
	return s.repo.List(ctx, limit, offset)
}

// Delete removes a link by code, returning ErrNotFound if it did not exist.
func (s *LinkService) Delete(ctx context.Context, code string) error {
	err := s.repo.Delete(ctx, code)
	if errors.Is(err, repository.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// Resolve records an access event and returns the target URL for a code. It is
// used by the redirect endpoint. Returns ErrNotFound if the code does not exist.
func (s *LinkService) Resolve(ctx context.Context, code, referer, userAgent string) (string, error) {
	url, err := s.repo.RecordVisit(ctx, code, repository.Event{
		Code:       code,
		AccessedAt: time.Now(),
		Referer:    referer,
		UserAgent:  userAgent,
	})
	if errors.Is(err, repository.ErrNotFound) {
		return "", ErrNotFound
	}
	return url, err
}

// ListEvents returns a page of access events for a code. It returns ErrNotFound
// if the link does not exist (distinct from a link that simply has no events).
func (s *LinkService) ListEvents(ctx context.Context, code string, limit, offset int) ([]repository.Event, int64, error) {
	_, err := s.repo.Get(ctx, code)
	if errors.Is(err, repository.ErrNotFound) {
		return nil, 0, ErrNotFound
	}
	if err != nil {
		return nil, 0, err
	}
	return s.repo.ListEvents(ctx, code, limit, offset)
}

// validateURL trims raw and verifies it is an http(s) URL with a host,
// returning the normalized form. Pure: no I/O, no dependencies.
func validateURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", ErrInvalidURL
	}
	return raw, nil
}

// validateAlias checks a custom alias against the allowed format. Pure.
func validateAlias(alias string) error {
	if !aliasPattern.MatchString(alias) {
		return ErrInvalidAlias
	}
	return nil
}
