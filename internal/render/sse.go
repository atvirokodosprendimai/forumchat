package render

import (
	"net/http"

	"github.com/a-h/templ"
	datastar "github.com/starfederation/datastar-go/datastar"
)

func NewSSE(w http.ResponseWriter, r *http.Request) *datastar.ServerSentEventGenerator {
	return datastar.NewSSE(w, r)
}

func PatchTempl(sse *datastar.ServerSentEventGenerator, c templ.Component, opts ...datastar.PatchElementOption) error {
	return sse.PatchElementTempl(c, opts...)
}
