import axios from 'axios'

export default class SettingsService {
  get() {
    return axios.get('/settings').then((r) => r.data)
  }

  update(settings) {
    return axios.put('/settings', JSON.stringify(settings)).then((r) => r.data)
  }
}
