package adminapi

import (
	"net/http"
	"sort"

	"obsdatalayer/internal/fanout"
)

// operationalCatalogDoc describes the operational endpoints the gateway serves,
// so the admin UI can render a button per endpoint without keeping its own copy
// of the table.
//
// The SPA previously hard-coded both the alias list and the backend-to-mount
// map. That is two sources of truth for facts the gateway already knows, and
// they drift silently: an alias renamed in Go leaves a button that 404s, and a
// grant regraded leaves a tooltip naming the wrong permission. Serving the
// table means the page can only ever offer what the gateway actually registers.
type operationalCatalogDoc struct {
	// Mounts maps a backend to the path its routes are served under, mirroring
	// fanout.BackendMounts.
	Mounts map[string]string `json:"mounts"`
	// Endpoints maps a backend to its allowlisted endpoints, in table order.
	Endpoints map[string][]operationalEndpointDoc `json:"endpoints"`
}

type operationalEndpointDoc struct {
	Alias string `json:"alias"`
	// Action is the grant a data-plane caller needs. The admin listener does
	// not require it -- an admin grant covers these already -- but the UI shows
	// it so an operator can tell a consumer what to ask for.
	Action string `json:"action"`
}

func (h *handler) getOperationalCatalog(w http.ResponseWriter, r *http.Request) {
	doc := operationalCatalogDoc{
		Mounts:    make(map[string]string, len(fanout.BackendMounts)),
		Endpoints: make(map[string][]operationalEndpointDoc, len(fanout.BackendMounts)),
	}

	backends := make([]string, 0, len(fanout.BackendMounts))
	for backend := range fanout.BackendMounts {
		backends = append(backends, backend)
	}
	sort.Strings(backends)

	for _, backend := range backends {
		doc.Mounts[backend] = fanout.BackendMounts[backend]
		endpoints := fanout.OperationalEndpoints(backend)
		out := make([]operationalEndpointDoc, 0, len(endpoints))
		for _, e := range endpoints {
			out = append(out, operationalEndpointDoc{Alias: e.Alias, Action: e.Action})
		}
		doc.Endpoints[backend] = out
	}

	writeJSON(w, http.StatusOK, doc)
}
