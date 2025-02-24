//go:build controld

package controld

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

const utilityURL = "https://api.controld.com/utility"

func TestFetchResolverConfig(t *testing.T) {
	tests := []struct {
		name    string
		uid     string
		wantErr bool
	}{
		{"valid", "p2", false},
		{"invalid uid", "abcd1234", true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FetchResolverConfig(tc.uid)
			assert.False(t, (err != nil) != tc.wantErr)
			if !tc.wantErr {
				assert.NotEmpty(t, got.DOH)
			}
		})
	}
}
