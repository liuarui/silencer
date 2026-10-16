package gateway

import (
	"net/http"
	"strings"

	"golang.org/x/net/http/httpguts"
)

// Extra provider headers must not alter routing/framing or reintroduce console
// credentials. Both probes and live requests use the same injection rules.
func applyUpstreamHeaders(req *http.Request, headers map[string]string) {
	for name, value := range headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			continue
		}
		switch strings.ToLower(name) {
		case "host", "content-length", "transfer-encoding", "connection", "proxy-connection",
			"keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "upgrade":
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "x-silencer-") {
			continue
		}
		req.Header.Set(name, value)
	}
	stripGatewayCredentials(req)
}
