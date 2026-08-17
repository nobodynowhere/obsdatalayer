import axios from 'axios'

export default class ConfigService {
  // Returns YAML text; basic_auth values are redacted by the gateway.
  get() {
    return axios.get('/api/config', { headers: { Accept: 'application/yaml' } }).then((r) => r.data)
  }

  reload() {
    return axios.post('/api/config/reload').then((r) => r.data)
  }

  whoami() {
    return axios.get('/api/whoami').then((r) => r.data)
  }
}
