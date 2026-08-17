import axios from 'axios'

export default class SettingsService {
  get() {
    return axios.get('/api/settings').then((r) => r.data)
  }

  update(settings) {
    return axios.put('/api/settings', JSON.stringify(settings)).then((r) => r.data)
  }
}
