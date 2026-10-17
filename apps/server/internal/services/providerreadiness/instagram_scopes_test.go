package providerreadiness

import (
	"slices"
	"testing"

	"github.com/openpost/backend/internal/capabilities"
)

func TestRequiredScopesForInstagramDependOnConnectionKind(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		accountKind string
		want        []string
	}{
		{"instagram login", "instagram_login", []string{"instagram_business_basic", "instagram_business_content_publish"}},
		{"facebook page connection", "business", []string{"instagram_basic", "instagram_content_publish"}},
		{"unspecified connection", "standard", []string{"instagram_basic", "instagram_content_publish"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := RequiredScopesForSubject(Subject{
				Provider: capabilities.ProviderInstagram, AccountKind: test.accountKind,
				Operation: OperationPublishImmediate,
			})
			if !slices.Equal(got, test.want) {
				t.Fatalf("scopes = %v, want %v", got, test.want)
			}
		})
	}
}
