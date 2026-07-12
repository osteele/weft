package dataloc

import (
	"reflect"
	"testing"
)

func TestFilterAutoDetectedInputsExplicitWins(t *testing.T) {
	got := FilterAutoDetectedInputs(
		[]string{"hf:hf-internal-testing/tiny-random-llama", "hf:Qwen/Qwen2.5-7B"},
		[]string{"hf:hf-internal-testing/tiny-random-LlamaForCausalLM"},
	)
	if len(got) != 0 {
		t.Fatalf("FilterAutoDetectedInputs = %v, want empty when explicit inputs are declared", got)
	}
}

func TestFilterAutoDetectedInputsRequiresRepoShape(t *testing.T) {
	got := FilterAutoDetectedInputs(
		[]string{"hf:gpt2", "hf:Qwen/Qwen2.5-7B", "hf-dataset:wikitext", "hf-dataset:allenai/c4"},
		nil,
	)
	want := []string{"hf:Qwen/Qwen2.5-7B", "hf-dataset:allenai/c4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FilterAutoDetectedInputs = %v, want %v", got, want)
	}
}

func TestFilterAutoDetectedInputsRejectsDeclaredSubstring(t *testing.T) {
	if autoDetectedInputAllowed("hf:tiny-random-llama", []string{"hf:hf-internal-testing/tiny-random-LlamaForCausalLM"}) {
		t.Fatal("autoDetectedInputAllowed accepted substring of declared HF repo")
	}
}
