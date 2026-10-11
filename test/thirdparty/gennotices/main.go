// Command gennotices rewrites THIRD_PARTY_NOTICES from the module graph
// (make notices, run from the repo root).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/eunmann/mro2nf/test/thirdparty"
)

func main() {
	if err := thirdparty.Write(context.Background(), "."); err != nil {
		fmt.Fprintln(os.Stderr, "gennotices:", err)
		os.Exit(1)
	}
}
