package gateway

type routeReadiness struct {
	Route    string `json:"route"`
	Ready    bool   `json:"ready"`
	Backends int    `json:"backends"`
	Healthy  int    `json:"healthy"`
	Eligible int    `json:"eligible"`
	Open     int    `json:"open_circuits"`
	Draining int    `json:"draining"`
}

func (g *Gateway) Ready() bool {
	return routesReady(g.readinessSnapshot(g.routes.Load()), g.maintenance.Load())
}

func (g *Gateway) readinessSnapshot(set *routeSet) []routeReadiness {
	result := make([]routeReadiness, 0, len(set.Routes))
	for _, route := range set.Routes {
		state := routeReadiness{Route: route.Config.Name}
		for _, backend := range route.Balancer.Backends() {
			state.Backends++
			if backend.Healthy.Load() {
				state.Healthy++
			}
			if backend.Draining.Load() {
				state.Draining++
			}
			if backend.Breaker.State().String() == "open" {
				state.Open++
			}
			if !backend.Draining.Load() && backend.Healthy.Load() && backend.Breaker.Available() {
				state.Eligible++
			}
		}
		state.Ready = state.Eligible > 0
		result = append(result, state)
	}
	return result
}

func routesReady(routes []routeReadiness, maintenance bool) bool {
	if maintenance {
		return false
	}
	for _, route := range routes {
		if !route.Ready {
			return false
		}
	}
	return true
}
