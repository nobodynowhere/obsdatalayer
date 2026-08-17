import axios from 'axios'

export default class ConfigService {
  // Returns YAML text; basic_auth values are redacted by the gateway.
  get() {
    return axios.get('/config', { headers: { Accept: 'application/yaml' } }).then((r) => r.data)
  }

  reload() {
    return axios.post('/config/reload').then((r) => r.data)
  }

  whoami() {
    return axios.get('/whoami').then((r) => r.data)
  }
}
