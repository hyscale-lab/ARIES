package benchmark

import "os"

// environmentAPIKeyLookup matches the application default for judge credentials.
func environmentAPIKeyLookup(name string) ([]byte, bool) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil, false
	}
	return []byte(value), true
}
