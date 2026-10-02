package prompt_test

import (
	"fmt"
	"kagari/internal/agent/prompt"
	"testing"
)

func TestPromptCompose(t *testing.T) {
	fmt.Printf("%s", prompt.GetPromptTemplate())
}
