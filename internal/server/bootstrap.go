package server

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/deweysasser/whisper-network/internal/auth"
	"github.com/deweysasser/whisper-network/internal/store"
)

// Bootstrap ensures the "public" queue and the "admin" user exist (DESIGN.md
// D7). When it creates the admin it uses password, or generates a random one
// if password is empty, and writes the password once to out together with
// instructions. The admin must change it at first login.
//
// out is meant to be the terminal (stderr), deliberately not the JSONL log:
// logs are shipped, indexed and retained, and a credential must never reach
// them (DESIGN.md §10.1). It returns whether the admin was created.
func Bootstrap(ctx context.Context, st *store.Store, password string, params auth.PasswordParams, out io.Writer) (bool, error) {
	_, err := st.UserByUsername(ctx, store.AdminUsername)
	switch {
	case err == nil:
		_, err = st.Bootstrap(ctx, "unused: admin exists") // still ensures the public queue
		return false, err
	case !errors.Is(err, store.ErrNotFound):
		return false, err
	}
	generated := password == ""
	if generated {
		if password, err = auth.GeneratePassword(); err != nil {
			return false, err
		}
	}
	hash, err := auth.HashPassword(password, params)
	if err != nil {
		return false, err
	}
	created, err := st.Bootstrap(ctx, hash)
	if err != nil || !created {
		return created, err
	}
	if generated {
		_, err = fmt.Fprintf(out, "\nwhisper-network: created user %q with password:\n\n    %s\n\n"+
			"This is shown only once. You must change it at first login.\n\n", store.AdminUsername, password)
	} else {
		_, err = fmt.Fprintf(out, "whisper-network: created user %q with the configured admin password.\n",
			store.AdminUsername)
	}
	return true, err
}
