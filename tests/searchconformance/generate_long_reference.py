#!/usr/bin/env python3
"""Generate Unicode 15 long-segment reference cases in a fresh local file."""

import json
from pathlib import Path
import random
import sys
import unicodedata

if unicodedata.unidata_version != '15.0.0':
    raise RuntimeError('The independent oracle must use Unicode 15.0.0')
if len(sys.argv) != 2:
    raise SystemExit('usage: search-long-normalization-oracle.py <new-output.json>')

SPACE = set('\t\n\v\f\r \u0085      　')
SPACE.update(chr(value) for value in range(0x2000, 0x200b))
MAX_INPUT = 16 << 10
MAX_OUTPUT = 64 << 10
CASES = []


def add(name, text):
    if len(text.encode('utf-8')) > MAX_INPUT:
        raise ValueError(f'oversized generated input: {name}')
    first = unicodedata.normalize('NFKC', text)
    folded = first.casefold()
    second = unicodedata.normalize('NFKC', folded)
    if any(len(item.encode('utf-8')) > MAX_OUTPUT for item in (first, folded, second)):
        CASES.append({'name': name, 'input': text, 'error': 'limit'})
        return
    start = 0
    end = len(second)
    while start < end and second[start] in SPACE:
        start += 1
    while end > start and second[end - 1] in SPACE:
        end -= 1
    CASES.append({'name': name, 'input': text, 'normalized': second[start:end]})


for count in [0, 1, 23, 24, 29, 30, 31, 64, 256, 1024, 4000]:
    add(f'repeated-{count}', 'A' + '̀' * count)
    add(f'initial-{count}', '̀' * count + 'Z')
    if 1 + count * 8 <= MAX_INPUT:
        add(f'descending-{count}', 'A' + ('̴̧̖̕' * count))
add('descending-maximum', 'A' + '̴̧̖̕' * 2047)
add('hangul-and-long-marks', '각' + '̀̕' * 700 + '나')
add('unicode-whitespace', '\u0085 Straßé̀' + '̕' * 200 + '　')
add('version-boundary', 'A꟱B' + '̀̕' * 200)
add('expansion-limit', '㌖' * 5460)
randomizer = random.Random(0x20261117)
starters = ['A', 'e', 'ß', 'İ', 'Σ', '台', '㌖', '가', '😀', '']
marks = ['̀', '́', '̕', '̖', '̧', '̴', 'ͅ']
for index in range(500):
    count = randomizer.randrange(25, 1200)
    text = randomizer.choice(starters) + ''.join(randomizer.choices(marks, k=count))
    if randomizer.randrange(3) == 0:
        text += randomizer.choice(starters)
    add(f'deterministic-{index}', text)

out = Path(sys.argv[1])
with out.open('x', encoding='utf-8') as stream:
    json.dump({'unicode': unicodedata.unidata_version, 'cases': CASES}, stream, ensure_ascii=False, separators=(',', ':'))
print(f'Generated {len(CASES)} Unicode 15 cases in {out}')
