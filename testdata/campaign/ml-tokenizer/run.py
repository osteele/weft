"""Test HF tokenizer download + inference."""

import json
import os
import time

import torch
from transformers import AutoTokenizer

tokenizer = AutoTokenizer.from_pretrained("distilbert-base-uncased")
tokens = tokenizer("Hello from weft test campaign", return_tensors="pt")

os.makedirs("output", exist_ok=True)
json.dump(
    {
        "model": "distilbert-base-uncased",
        "token_count": tokens["input_ids"].shape[1],
        "gpu": torch.cuda.get_device_name(0) if torch.cuda.is_available() else "none",
        "ts": time.time(),
    },
    open("output/result.json", "w"),
    indent=2,
)
print("ml-tokenizer: done")
