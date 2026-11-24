package service

import (
	"encoding/json"
	"errors"

	"github.com/tidwall/gjson"
)

// JSON syntax alone is insufficient: upstream gateways sometimes return an
// error or a different protocol's envelope with HTTP 200. Require the Messages
// discriminator before committing a successful synchronous response. Leave
// provider-specific Message fields and extensions untouched.
func validateAnthropicMessageResponse(body []byte) error {
	if !json.Valid(body) {
		return errors.New("invalid JSON in upstream Messages response")
	}
	root := gjson.ParseBytes(body)
	if !root.IsObject() || root.Get("type").Str != "message" || root.Get("error").IsObject() {
		return errors.New("upstream JSON is not an Anthropic Message")
	}
	return nil
}
