// Quickstart exchanges a credential for a token and prints whoami.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/21stware/raise-go"
)

func main() {
	c := raise.New(os.Getenv("RAISE_CLIENT_ID"), os.Getenv("RAISE_CLIENT_SECRET"),
		raise.WithBaseURL(os.Getenv("RAISE_BASE_URL")),
		raise.WithTenant(os.Getenv("RAISE_TENANT")),
	)
	me, err := c.Platform.Whoami(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("tenant=%s app=%s credential=%s scopes=%v\n", me.Tenant, me.App, me.Credential, me.Scopes)
}
