package recorder

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// The formats of the trending WebSocket and of /proxy/filterTokens were not
// captured yet, so tokens are found by shape: raw messages are stored as is
// and these heuristics only decide which tokens to follow.

var addressKeys = map[string]bool{
	"address": true, "tokenaddress": true, "token_address": true, "mint": true,
	"tokenid": true, "token_id": true, "id": true,
}

var networkKeys = []string{"networkId", "network_id", "networkID", "chainId", "chain_id"}

// solanaMints returns Solana token addresses found in a JSON document, in
// document order for arrays, without duplicates.
func solanaMints(raw []byte, networkID string) []string {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e)
			}
		case map[string]any:
			if m := objectMint(x, networkID); m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
			for _, k := range sortedKeys(x) {
				walk(x[k])
			}
		}
	}
	walk(v)
	return out
}

func objectMint(obj map[string]any, networkID string) string {
	if net, ok := objectNetwork(obj); ok && net != networkID {
		return ""
	}
	for _, k := range sortedKeys(obj) {
		if !addressKeys[strings.ToLower(k)] {
			continue
		}
		s, ok := obj[k].(string)
		if !ok {
			continue
		}
		if addr, net, found := strings.Cut(s, ":"); found {
			if net == networkID && isBase58Address(addr) {
				return addr
			}
			continue
		}
		if isBase58Address(s) {
			return s
		}
	}
	return ""
}

func objectNetwork(obj map[string]any) (string, bool) {
	for _, k := range networkKeys {
		switch v := obj[k].(type) {
		case json.Number:
			return v.String(), true
		case string:
			return v, true
		}
	}
	return "", false
}

// isBase58Address matches Solana public keys: 32-44 base58 characters. EVM
// addresses (0x...) and UUIDs never match.
func isBase58Address(s string) bool {
	if len(s) < 32 || len(s) > 44 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '1' && c <= '9', c >= 'A' && c <= 'H', c >= 'J' && c <= 'N', c >= 'P' && c <= 'Z',
			c >= 'a' && c <= 'k', c >= 'm' && c <= 'z':
		default:
			return false
		}
	}
	return true
}

// splitByToken assigns the elements of a batch response to the requested
// tokens: an element belongs to the first requested mint it mentions.
func splitByToken(raw json.RawMessage, mints []string) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for _, el := range batchElements(raw) {
		for _, m := range mints {
			if _, done := out[m]; !done && bytes.Contains(el, []byte(m)) {
				out[m] = el
				break
			}
		}
	}
	return out
}

// batchElements finds the list of per-token objects: the response itself if
// it is an array, otherwise the largest array or keyed object inside it.
func batchElements(raw json.RawMessage) []json.RawMessage {
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return arr
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var best []json.RawMessage
	for _, k := range sortedKeys(obj) {
		var inner []json.RawMessage
		if json.Unmarshal(obj[k], &inner) == nil {
			if len(inner) > len(best) {
				best = inner
			}
			continue
		}
		if nested := batchElements(obj[k]); len(nested) > len(best) {
			best = nested
		}
	}
	if len(best) > 0 {
		return best
	}
	// An object keyed by token id: {"<mint>:<net>": {...}, ...}.
	for _, k := range sortedKeys(obj) {
		if isBase58Address(strings.SplitN(k, ":", 2)[0]) {
			best = append(best, append(json.RawMessage(`{"key":`+quote(k)+`,"value":`), append(obj[k], '}')...))
		}
	}
	return best
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }
