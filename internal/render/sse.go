package render

import (
	"net/http"

	"github.com/a-h/templ"
	datastar "github.com/starfederation/datastar-go/datastar"

	"github.com/atvirokodosprendimai/forumchat/internal/httpx"
)

// NewSSE primes SSE headers + status before delegating to datastar.NewSSE.
// See httpx.PrimeSSE for why the prime step is required when chi's compressor
// (or any wrapping ResponseWriter) sits between the handler and the socket.
func NewSSE(w http.ResponseWriter, r *http.Request) *datastar.ServerSentEventGenerator {
	httpx.PrimeSSE(w)
	return datastar.NewSSE(w, r)
}

func PatchTempl(sse *datastar.ServerSentEventGenerator, c templ.Component, opts ...datastar.PatchElementOption) error {
	return sse.PatchElementTempl(c, opts...)
}
