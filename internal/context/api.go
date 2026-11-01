package context

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/cockroachdb/errors"
	"github.com/unknwon/paginater"
	"gopkg.in/macaron.v1"
	log "unknwon.dev/clog/v2"

	"gogs.io/gogs/internal/conf"
	"gogs.io/gogs/internal/errx"
)

type APIContext struct {
	*Context // TODO: Reduce to only needed fields instead of full shadow

	// Base URL for the version of API endpoints, e.g. https://try.gogs.io/api/v1
	BaseURL string

	Org *APIOrganization
}

// FIXME: move this constant to github.com/gogs/go-gogs-client
const DocURL = "https://github.com/gogs/docs-api"

// NoContent renders the 204 response.
func (c *APIContext) NoContent() {
	c.Status(http.StatusNoContent)
}

// NotFound renders the 404 response.
func (c *APIContext) NotFound() {
	c.Status(http.StatusNotFound)
}

// ErrorStatus renders error with given status code.
func (c *APIContext) ErrorStatus(status int, err error) {
	c.JSON(status, map[string]string{
		"message": err.Error(),
		"url":     DocURL,
	})
}

// Error renders the 500 response.
func (c *APIContext) Error(err error, msg string) {
	log.ErrorDepth(4, "%s: %v", msg, err)
	c.ErrorStatus(
		http.StatusInternalServerError,
		errors.New("Something went wrong, please check the server logs for more information."),
	)
}

// Errorf renders the 500 response with formatted message.
func (c *APIContext) Errorf(err error, format string, args ...any) {
	c.Error(err, fmt.Sprintf(format, args...))
}

// NotFoundOrError use error check function to determine if the error
// is about not found. It responses with 404 status code for not found error,
// or error context description for logging purpose of 500 server error.
func (c *APIContext) NotFoundOrError(err error, msg string) {
	if errx.IsNotFound(err) {
		c.NotFound()
		return
	}
	c.Error(err, msg)
}

// SetLinkHeader sets pagination link header by given total number and page size.
// Query parameters other than "page" are kept in the links.
func (c *APIContext) SetLinkHeader(total, pageSize int) {
	page := paginater.New(total, pageSize, c.QueryInt("page"), 0)
	link := func(number int, rel string) string {
		query := c.Req.URL.Query()
		query.Set("page", strconv.Itoa(number))
		return fmt.Sprintf("<%s%s?%s>; rel=\"%s\"", conf.Server.ExternalURL, c.Req.URL.Path[1:], query.Encode(), rel)
	}

	links := make([]string, 0, 4)
	if page.HasNext() {
		links = append(links, link(page.Next(), "next"))
	}
	if !page.IsLast() {
		links = append(links, link(page.TotalPages(), "last"))
	}
	if !page.IsFirst() {
		links = append(links, link(1, "first"))
	}
	if page.HasPrevious() {
		links = append(links, link(page.Previous(), "prev"))
	}

	if len(links) > 0 {
		c.Header().Set("Link", strings.Join(links, ","))
	}
}

func APIContexter() macaron.Handler {
	return func(ctx *Context) {
		c := &APIContext{
			Context: ctx,
			BaseURL: conf.Server.ExternalURL + "api/v1",
		}
		ctx.Map(c)
	}
}
