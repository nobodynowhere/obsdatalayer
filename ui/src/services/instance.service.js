import axios from 'axios'

const API = '/api/instances'

export default class InstanceService {
  list() {
    return axios.get(API).then((r) => r.data.instances ?? [])
  }

  get(name) {
    return axios.get(`${API}/${encodeURIComponent(name)}`).then((r) => r.data)
  }

  create(instance) {
    return axios.post(API, JSON.stringify(instance)).then((r) => r.data)
  }

  update(name, instance) {
    return axios.put(`${API}/${encodeURIComponent(name)}`, JSON.stringify(instance)).then((r) => r.data)
  }

  remove(name) {
    return axios.delete(`${API}/${encodeURIComponent(name)}`).then((r) => r.data)
  }

  // The catalog of operational endpoints, and the mount each backend answers
  // under, both come from the gateway. Mirroring either here would be a second
  // source of truth for something the server already knows, and it would drift
  // silently: a renamed alias leaves a button that 404s, a regraded action
  // leaves a tooltip naming the wrong grant.
  operationalCatalog() {
    return axios.get('/api/operational-endpoints').then((r) => r.data)
  }

  // targetStatus asks one endpoint of every target at once. The gateway answers
  // 200 with a per-target result whenever it managed to ask, so a target that
  // is down shows up as an entry with an error rather than as a failed call.
  targetStatus(mount, name, endpoint) {
    if (!mount) return Promise.reject(new Error('no mount is configured for this backend'))
    return axios
      .get(`${mount}/targets/${encodeURIComponent(name)}/${encodeURIComponent(endpoint)}`, {
        validateStatus: () => true,
      })
      .then((r) => {
        if (r.status !== 200) {
          throw new Error(r.data?.error || `gateway returned ${r.status}`)
        }
        return r.data
      })
  }
}
