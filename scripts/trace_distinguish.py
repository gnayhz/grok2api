#!/usr/bin/env python3
"""Classify archived upstream SSE traces without printing prompt text."""
from __future__ import annotations

import collections
import json
import sys
from pathlib import Path

EMPTY_LIT = 'encrypted_content":""'

def parse_sse(path: Path) -> dict:
    raw = path.read_text(encoding='utf-8', errors='replace')
    t0 = ts = last = None
    types = []
    first = {}
    has_sum = has_text = has_think_delta = enc = enc_empty = done = False
    item_types = []
    n_data = huge = keepalive = 0
    for line in raw.splitlines():
        if line.startswith('#ts '):
            try:
                ts = int(line[4:])
            except ValueError:
                continue
            if t0 is None:
                t0 = ts
            continue
        if line.startswith(':'):
            keepalive += 1
            continue
        if not line.startswith('data:'):
            continue
        n_data += 1
        payload = line[5:].lstrip()
        if len(payload) > 10000:
            huge += 1
        if 'encrypted_content' in payload:
            if EMPTY_LIT in payload:
                enc_empty = True
            else:
                enc = True
        try:
            obj = json.loads(payload)
        except json.JSONDecodeError:
            types.append('UNPARSED')
            continue
        typ = obj.get('type') or '?'
        types.append(typ)
        rel = (ts - t0) if (ts is not None and t0 is not None) else 0
        first.setdefault('first_event', (typ, rel))
        item = obj.get('item') or {}
        if isinstance(item, dict) and item.get('type'):
            item_types.append(str(item.get('type')))
        if typ.endswith('reasoning_summary_text.delta') and obj.get('delta'):
            has_sum = True
            first.setdefault('summary', rel)
        if 'thinking' in typ and obj.get('delta'):
            has_think_delta = True
            first.setdefault('thinking', rel)
        delta = obj.get('delta')
        textish = False
        if typ == 'response.output_text.delta' and delta:
            textish = True
        elif typ == 'content_block_delta' and isinstance(delta, dict) and delta.get('text'):
            textish = True
        if textish:
            has_text = True
            first.setdefault('text', rel)
        if typ.endswith('output_item.done'):
            first.setdefault('item_done', rel)
        if typ.endswith('output_item.added'):
            first.setdefault('item_added', rel)
        if typ == 'response.created':
            first.setdefault('created', rel)
        if typ in ('response.completed', 'message_stop', 'response.done'):
            done = True
            first.setdefault('completed', rel)
        if ts is not None:
            last = ts
    dur = (last - t0) if last is not None and t0 is not None else 0
    return {
        'empty': n_data == 0,
        'uniq': tuple(dict.fromkeys(types[:12])),
        'first': first,
        'has_sum': has_sum,
        'has_text': has_text,
        'has_think_delta': has_think_delta,
        'enc': enc,
        'enc_empty': enc_empty,
        'done': done,
        'dur': dur,
        'huge': huge,
        'keepalive': keepalive,
        'n_data': n_data,
        'item_types': tuple(item_types[:8]),
        'raw_len': len(raw),
    }

def parse_req(path: Path) -> dict:
    try:
        obj = json.loads(path.read_text(encoding='utf-8', errors='replace'))
    except json.JSONDecodeError:
        return {}
    if not isinstance(obj, dict):
        return {'_type': type(obj).__name__}
    tools = obj.get('tools') or []
    tool_types = []
    if isinstance(tools, list):
        for tool in tools:
            if isinstance(tool, dict):
                tool_types.append(str(tool.get('type') or tool.get('name') or '?'))
    effort = None
    if isinstance(obj.get('reasoning'), dict):
        effort = obj['reasoning'].get('effort')
    effort = effort or obj.get('reasoning_effort')
    tool_choice = obj.get('tool_choice')
    if isinstance(tool_choice, dict):
        tool_choice = tool_choice.get('type')
    return {
        'keys': tuple(sorted(obj.keys())),
        'model': obj.get('model'),
        'stream': obj.get('stream'),
        'tool_types': tuple(tool_types),
        'tool_choice': tool_choice,
        'effort': effort,
        'n_messages': len(obj.get('messages') or obj.get('input') or []),
        'has_instructions': bool(obj.get('instructions')),
    }

def pair_files(directory: Path) -> dict:
    by_ts = collections.defaultdict(dict)
    for path in directory.iterdir():
        parts = path.name.split('_')
        if len(parts) < 3:
            continue
        rec = by_ts[parts[0]]
        rec['op'] = parts[2]
        rec['model'] = parts[3]
        if path.name.endswith('.sse'):
            rec['sse'] = path
        elif path.name.endswith('stream.req.json') or path.name.endswith('body.req.json'):
            rec['req'] = path
        elif path.name.endswith('body.json'):
            rec['body'] = path
    return by_ts

def classify(row: dict) -> str:
    if row.get('empty'):
        return 'P0-empty'
    if row['has_sum'] and row['done']:
        return 'clean'
    if row['has_sum'] and not row['done']:
        return 'cut-after-think'
    if row['enc'] and not row['has_sum'] and not row['has_text']:
        first_ms = (row['first'].get('first_event') or (None, 9999))[1]
        return 'D-a' if first_ms is not None and first_ms <= 50 else 'D-b'
    if row['has_text'] and not row['has_sum']:
        return 'outrun'
    if row['has_think_delta']:
        return 'think-delta-other'
    if row['done'] and not row['has_sum'] and not row['has_text']:
        return 'empty-terminal'
    return 'other'

def pct(values, p):
    values = sorted(values)
    return values[min(len(values) - 1, int(p / 100 * (len(values) - 1)))]

def summarize_times(rows, key):
    xs = []
    for row in rows:
        if key == 'first':
            value = (row['first'].get('first_event') or (None, None))[1]
        else:
            value = row['first'].get(key)
        if value is not None:
            xs.append(value)
    if not xs:
        return 'n/a'
    xs.sort()
    return 'n=%d min=%s p50=%s p95=%s max=%s' % (len(xs), xs[0], pct(xs, 50), pct(xs, 95), xs[-1])

def main():
    directory = Path(sys.argv[1] if len(sys.argv) > 1 else '/tmp/upstream-traces-8003')
    rows = []
    for ts, rec in pair_files(directory).items():
        row = {'ts': ts, 'op': rec.get('op'), 'model': rec.get('model')}
        if 'sse' in rec:
            row.update(parse_sse(rec['sse']))
            row['has_sse'] = True
        else:
            row['has_sse'] = False
            row['empty'] = True
        if 'req' in rec:
            row['req'] = parse_req(rec['req'])
        rows.append(row)
    sse_rows = [row for row in rows if row.get('has_sse')]
    print('pairs', len(rows), 'sse', len(sse_rows))
    grouped = collections.defaultdict(list)
    for row in sse_rows:
        grouped[classify(row)].append(row)
    print()
    print('CLASS')
    for name, items in sorted(grouped.items(), key=lambda item: -len(item[1])):
        print('  %5d  %s' % (len(items), name))
    print()
    print('BY OP x CLASS')
    counts = collections.Counter((row['op'], classify(row)) for row in sse_rows)
    for key, value in counts.most_common(40):
        print('  %5d  %s' % (value, key))
    print()
    print('BY MODEL x CLASS')
    for name in ('D-a', 'D-b', 'outrun', 'P0-empty', 'clean'):
        models = collections.Counter(row['model'] for row in grouped.get(name, []))
        print(name, dict(models))
    print()
    print('EVENT SEQ by class')
    for name in ('clean', 'D-a', 'D-b', 'outrun', 'P0-empty', 'other', 'cut-after-think', 'empty-terminal'):
        seqs = collections.Counter(row['uniq'] for row in grouped.get(name, []))
        print()
        print('== %s n=%d ==' % (name, len(grouped.get(name, []))))
        for seq, value in seqs.most_common(6):
            print('  %4d  %s' % (value, seq))
    print()
    print('TIMING ms')
    for name in ('clean', 'D-a', 'D-b', 'outrun'):
        items = grouped.get(name, [])
        durs = sorted(row['dur'] for row in items)
        dur_p50 = durs[len(durs) // 2] if durs else 'n/a'
        print(' %-12s first=%s created=%s summary=%s item_done=%s text=%s dur_p50=%s' % (
            name,
            summarize_times(items, 'first'),
            summarize_times(items, 'created'),
            summarize_times(items, 'summary'),
            summarize_times(items, 'item_done'),
            summarize_times(items, 'text'),
            dur_p50,
        ))
    print()
    print('REQ KEYS by class')
    for name in ('clean', 'D-a', 'D-b', 'outrun', 'P0-empty'):
        items = [row for row in grouped.get(name, []) if row.get('req')]
        keys = collections.Counter(row['req'].get('keys') for row in items)
        tools = collections.Counter(row['req'].get('tool_types') for row in items)
        effort = collections.Counter(str(row['req'].get('effort')) for row in items)
        print()
        print('== %s ==' % name)
        print('  keys', keys.most_common(3))
        print('  tools', tools.most_common(6))
        print('  effort', effort.most_common(8))
    print()
    print('OTHER uniq seqs')
    for seq, value in collections.Counter(row['uniq'] for row in grouped.get('other', [])).most_common(15):
        print('  %4d  %s' % (value, seq))
    print()
    print('D-a/D-b first event type')
    for name in ('D-a', 'D-b'):
        first_types = collections.Counter((row['first'].get('first_event') or ('?', 0))[0] for row in grouped.get(name, []))
        print(name, first_types.most_common())
    clean = grouped.get('clean', [])
    degraded = grouped.get('D-a', []) + grouped.get('D-b', [])
    print()
    print('enc on clean', sum(1 for row in clean if row['enc']), '/', len(clean))
    print('huge lines D', sum(row['huge'] for row in degraded))
    print('huge lines clean', sum(row['huge'] for row in clean))
    print('item_types D-a', collections.Counter(row['item_types'] for row in grouped.get('D-a', [])).most_common(5))
    print('item_types clean', collections.Counter(row['item_types'] for row in clean).most_common(5))
    print('keepalive D', collections.Counter(row['keepalive'] > 0 for row in degraded))
    print('keepalive clean', collections.Counter(row['keepalive'] > 0 for row in clean))

if __name__ == '__main__':
    main()
