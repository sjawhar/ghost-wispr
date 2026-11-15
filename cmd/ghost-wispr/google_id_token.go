package main

import (
	"context"
	"flag"
	"fmt"
	"io"
)

type idTokenMinter func(context.Context, string, string) (string, error)

func runGoogleIDTokenCommand(ctx context.Context, args []string, output io.Writer, credentialPath string, mint idTokenMinter) (bool, error) {
	if len(args) == 0 || args[0] != "google-id-token" {
		return false, nil
	}

	flags := flag.NewFlagSet("google-id-token", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	audience := flags.String("audience", "", "Google ID token audience")
	credentials := flags.String("credentials", credentialPath, "Google credential file")
	if err := flags.Parse(args[1:]); err != nil {
		return true, err
	}
	if flags.NArg() != 0 {
		return true, fmt.Errorf("google-id-token accepts no positional arguments")
	}
	if *audience == "" {
		return true, fmt.Errorf("google-id-token requires --audience")
	}

	token, err := mint(ctx, *credentials, *audience)
	if err != nil {
		return true, err
	}
	_, err = fmt.Fprintln(output, token)
	return true, err
}
