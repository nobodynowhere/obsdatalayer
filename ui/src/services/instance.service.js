import axios from 'axios'

const API = '/instances'

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
}
