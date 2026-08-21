import axios from 'axios'

const API = '/api/users'

export default class UserService {
  list() {
    return axios.get(API).then((r) => r.data.users ?? [])
  }

  get(name) {
    return axios.get(`${API}/${encodeURIComponent(name)}`).then((r) => r.data)
  }

  create(user) {
    return axios.post(API, JSON.stringify(user)).then((r) => r.data)
  }

  remove(name) {
    return axios.delete(`${API}/${encodeURIComponent(name)}`).then((r) => r.data)
  }

  setPassword(name, password) {
    return axios
      .put(`${API}/${encodeURIComponent(name)}/password`, JSON.stringify({ password }))
      .then((r) => r.data)
  }

  setRoles(name, roles) {
    return axios
      .put(`${API}/${encodeURIComponent(name)}/roles`, JSON.stringify({ roles }))
      .then((r) => r.data)
  }

  listApiKeys(name) {
    return axios.get(`${API}/${encodeURIComponent(name)}/apikeys`).then((r) => r.data ?? [])
  }

  // The response carries the only copy of the secret; it cannot be fetched again.
  createApiKey(name, label, expiresAt) {
    const body = { label }
    if (expiresAt) body.expires_at = expiresAt
    return axios
      .post(`${API}/${encodeURIComponent(name)}/apikeys`, JSON.stringify(body))
      .then((r) => r.data)
  }

  deleteApiKey(name, id) {
    return axios
      .delete(`${API}/${encodeURIComponent(name)}/apikeys/${encodeURIComponent(id)}`)
      .then((r) => r.data)
  }

  setGrants(name, grants) {
    return axios
      .put(`${API}/${encodeURIComponent(name)}/grants`, JSON.stringify({ grants }))
      .then((r) => r.data)
  }
}
