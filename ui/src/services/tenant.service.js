import axios from 'axios'

const API = '/tenants'

export default class TenantService {
  list() {
    return axios.get(API).then((r) => r.data.tenants ?? [])
  }

  get(id) {
    return axios.get(`${API}/${id}`).then((r) => r.data)
  }

  create(tenant) {
    return axios.post(API, JSON.stringify(tenant)).then((r) => r.data)
  }

  update(id, tenant) {
    return axios.put(`${API}/${id}`, JSON.stringify(tenant)).then((r) => r.data)
  }

  remove(id) {
    return axios.delete(`${API}/${id}`).then((r) => r.data)
  }
}
