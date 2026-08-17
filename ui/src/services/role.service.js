import axios from 'axios'

const API = '/api/roles'

export default class RoleService {
  list() {
    return axios.get(API).then((r) => r.data.roles ?? [])
  }

  get(name) {
    return axios.get(`${API}/${encodeURIComponent(name)}`).then((r) => r.data)
  }

  create(role) {
    return axios.post(API, JSON.stringify(role)).then((r) => r.data)
  }

  remove(name) {
    return axios.delete(`${API}/${encodeURIComponent(name)}`).then((r) => r.data)
  }

  setGrants(name, grants) {
    return axios
      .put(`${API}/${encodeURIComponent(name)}/grants`, JSON.stringify({ grants }))
      .then((r) => r.data)
  }
}
