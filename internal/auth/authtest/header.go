package authtest

import "encoding/base64"

// BasicHeader builds an HTTP Basic Authorization header value.
func BasicHeader(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}
