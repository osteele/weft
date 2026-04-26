package aiassist

import "fmt"

// Prompts are deliberately short. Stdin contains the output of `weft info`
// and `weft status` for the job — read it for a head start. Beyond that,
// rely on the project's CLAUDE.md, skills, and the weft CLI you already
// know how to drive from a normal terminal session.

const progressPrompt = `Summarize the progress of weft job wj%d for a developer's terminal UI.

Produce concise plain text (no JSON, no markdown fences):

- Line 1: one sentence — what the job is doing.
- Line 2: progress signal if visible (e.g., "Step 4200/10000", "epoch 3 of 10").
  If no progress markers are visible, say so — do not invent numbers.
- Line 3: rough ETA, or "ETA unknown".
- Up to 5 more lines: anything notable (warnings, slowdowns, recent errors).

Keep total output under 8 lines.`

const successPrompt = `Weft job wj%d finished successfully. Help the developer decide what to do next.

Output a single JSON object (no markdown fences, no preamble):

  {
    "summary": "<2-4 sentences on what was produced>",
    "choices": [
      {
        "id": "<short stable id>",
        "title": "<<= 60 chars, action verb first>",
        "description": "<1-3 lines>",
        "prompt": "<verbatim instructions for a follow-up agent invocation>"
      }
    ]
  }

Choices are 0-4 follow-up actions drawn from these categories, only when
applicable:

- Update the lab notebook (only if a notebook file exists; name it in the prompt).
- Update the paper (only if a paper draft exists and the result is paper-relevant;
  one choice per distinct section needing edits; name file + section in the prompt).
- Queue follow-up experiments (only if concrete next runs are warranted; each
  prompt must include a specific ` + "`weft run`" + ` command).

Verify files exist before naming them. Empty choices array is fine.`

const failurePrompt = `Weft job wj%d failed. Diagnose it and propose remediations.

Output a single JSON object (no markdown fences, no preamble):

  {
    "summary": "<2-5 sentences: root cause if identifiable, otherwise the
                most likely hypothesis and what evidence supports it>",
    "choices": [
      {
        "id": "<short stable id>",
        "title": "<<= 60 chars, fix described as an action>",
        "description": "<1-3 lines>",
        "prompt": "<verbatim instructions for a follow-up agent invocation;
                   include a concrete weft run command if retry is the fix>"
      }
    ]
  }

0-4 choices. Empty choices array is fine if you cannot identify a
plausible remediation — say why in the summary.`

func promptFor(k Kind, jobID int64) string {
	switch k {
	case KindProgress:
		return fmt.Sprintf(progressPrompt, jobID)
	case KindSuccess:
		return fmt.Sprintf(successPrompt, jobID)
	case KindFailure:
		return fmt.Sprintf(failurePrompt, jobID)
	}
	return fmt.Sprintf(progressPrompt, jobID)
}
