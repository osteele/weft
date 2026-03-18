#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = [
#     "torch",
#     "tqdm",
# ]
# ///
"""Minimal PyTorch training loop that produces tqdm and epoch progress output.

Used to verify that weft's TUI picks up progress from real training runs.
Trains a tiny MLP on synthetic data — runs in ~15s on CPU, ~5s on GPU.
"""

import torch
import torch.nn as nn
from torch.utils.data import DataLoader, TensorDataset
from tqdm import tqdm

device = torch.device("cuda" if torch.cuda.is_available() else "cpu")
print(f"Using device: {device}")

# Synthetic dataset
X = torch.randn(2000, 16, device=device)
y = (X[:, 0] + X[:, 1] * 0.5 > 0).long()
dataset = TensorDataset(X, y)
loader = DataLoader(dataset, batch_size=64, shuffle=True)

# Tiny model
model = nn.Sequential(nn.Linear(16, 32), nn.ReLU(), nn.Linear(32, 2)).to(device)
optimizer = torch.optim.Adam(model.parameters(), lr=1e-3)
criterion = nn.CrossEntropyLoss()

num_epochs = 5
for epoch in range(1, num_epochs + 1):
    total_loss = 0.0
    for batch_X, batch_y in tqdm(loader, desc=f"Epoch {epoch}/{num_epochs}"):
        optimizer.zero_grad()
        loss = criterion(model(batch_X), batch_y)
        loss.backward()
        optimizer.step()
        total_loss += loss.item()
    avg_loss = total_loss / len(loader)
    print(f"Epoch {epoch}/{num_epochs} complete — loss={avg_loss:.4f}")

print("Training complete!")
