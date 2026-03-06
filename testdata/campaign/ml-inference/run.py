"""Test small model GPU inference."""

import json
import os
import time

import torch
from transformers import AutoModel, AutoTokenizer

device = "cuda" if torch.cuda.is_available() else "cpu"
tokenizer = AutoTokenizer.from_pretrained("distilbert-base-uncased")
model = AutoModel.from_pretrained("distilbert-base-uncased").to(device)

inputs = tokenizer("Weft campaign test inference", return_tensors="pt").to(device)
with torch.no_grad():
    outputs = model(**inputs)

os.makedirs("output", exist_ok=True)
json.dump(
    {
        "model": "distilbert-base-uncased",
        "device": device,
        "hidden_size": outputs.last_hidden_state.shape[-1],
        "gpu_mem_mb": torch.cuda.max_memory_allocated() / 1e6
        if torch.cuda.is_available()
        else 0,
        "ts": time.time(),
    },
    open("output/result.json", "w"),
    indent=2,
)
print("ml-inference: done")
