package memory

import (
	"fmt"

	"carman/internal/secret"
)

// Requirement M6: nothing shaped like a credential is stored. Memory is read
// back into future sessions verbatim, so a secret written once would be
// replayed into every one of them.

// checkSecrets refuses the write when any field looks like a credential. The
// error names the kind of credential and never repeats the text.
func checkSecrets(fields ...string) error {
	for _, field := range fields {
		if kind, found := secret.Find(field); found {
			return fmt.Errorf("%w: the text looks like it contains %s", ErrSecret, kind)
		}
	}
	return nil
}
