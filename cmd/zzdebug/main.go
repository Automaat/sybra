package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/Automaat/sybra/internal/llmexec"
)

func main() {
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var variants []struct {
		Prompt   string `json:"prompt"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(data, &variants); err != nil {
		panic(err)
	}
	goldenData, err := os.ReadFile(os.Args[2])
	if err != nil {
		panic(err)
	}
	var cases []struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(goldenData, &cases); err != nil {
		panic(err)
	}
	prompt := variants[0].Prompt + "\n\n" + cases[0].Input
	out, err := llmexec.RunJSON(context.Background(), prompt, llmexec.Options{
		Provider: variants[0].Provider,
		Models:   map[string]string{variants[0].Provider: variants[0].Model},
	})
	if err != nil {
		panic(err)
	}
	fmt.Println("=== RAW OUTPUT ===")
	fmt.Println(out.Text)
	fmt.Println("=== END ===")
}
