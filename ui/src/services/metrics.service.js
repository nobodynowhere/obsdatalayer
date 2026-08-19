import axios from 'axios'

export default class MetricsService {
  // Aggregated gateway counters. The Prometheus exposition at /metrics remains
  // the source of truth for scraping; this is the JSON projection the overview
  // page renders.
  get() {
    return axios.get('/api/metrics').then((r) => r.data)
  }
}
