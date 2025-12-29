# Job Progress Reporting

The remote-jobs TUI can display progress for running jobs. To enable this, your job should periodically output progress lines to stdout or stderr.

## Supported Formats

```
Progress: 75%
Progress: 9/14
Progress: 9 of 14
Progress: 9 out of 14
```

The TUI scans the last portion of your job's output and displays the most recent progress line.

## Examples

### Shell Script

```bash
#!/bin/bash
total=10
for i in $(seq 1 $total); do
    echo "Progress: $i/$total"
    # ... do work ...
    sleep 1
done
```

### Python

```python
for i in range(100):
    print(f"Progress: {i+1}%")
    # ... do work ...
```

### Python with Steps

```python
steps = ["Loading data", "Training", "Evaluating", "Saving"]
for i, step in enumerate(steps, 1):
    print(f"Progress: {i} of {len(steps)}")
    print(f"Current step: {step}")
    # ... do work ...
```

## Display

- **List view**: Progress percentage shown in status column (e.g., "● 75%")
- **Details pane**: Progress bar with percentage and step info if available

```
Progress
  ████████████░░░░░░░░ 60%
  Step 6 of 10
```

## Notes

- Progress is parsed from the last ~500 lines of your job's output
- The TUI polls for updates every few seconds (configurable via `--log-refresh-interval`)
- Progress information is not persisted - it's only visible while the TUI is running
- The progress format is case-insensitive ("progress:" works too)
