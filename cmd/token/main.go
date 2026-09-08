package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/example/gatego/internal/auth"
)

func main() {
	sub := flag.String("sub", "demo-user", "JWT subject")
	role := flag.String("role", "developer", "JWT role")
	ttl := flag.Duration("ttl", time.Hour, "token TTL")
	flag.Parse()
	secret := os.Getenv("GATEGO_JWT_SECRET")
	if secret == "" {
		secret = "dev-only-change-me"
	}
	token, err := auth.Issue(secret, *sub, *role, *ttl)
	if err != nil {
		panic(err)
	}
	fmt.Println(token)
}
