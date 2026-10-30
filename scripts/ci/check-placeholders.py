#!/usr/bin/env python3
"""Fail if a UI input shows grey placeholder text that could pass for a value.

An empty gateway box reading "192.168.1.1" looked configured when it was
not. Placeholders may only be translated instructions from ALLOWED, never
example values.
Usage: scripts/ci/check-placeholders.py [ui src dir]
"""
import pathlib
import re
import sys

ALLOWED = {
    'smoothfs.field.uuidPlaceholder',
    'backups.field.unchanged',
    'benchmarks.net.filterServers',
    'plugins.detail.models.filename.placeholder',
}


def expression(text, start):
    """Return the JSX attribute value starting at text[start]."""
    if text[start] in '"\'':
        end = text.index(text[start], start + 1)
        return text[start:end + 1]
    depth = 0
    for i in range(start, len(text)):
        depth += {'{': 1, '}': -1}.get(text[i], 0)
        if depth == 0:
            return text[start:i + 1]
    return text[start:]


def allowed(expr):
    if not expr.startswith('{'):
        return False
    keys = re.findall(r"\bt\('([^']+)'\)", expr)
    rest = re.sub(r"\bt\('[^']+'\)", '', expr).replace("''", '')
    return bool(keys) and all(k in ALLOWED for k in keys) and not re.search('[\'"`]', rest)


def main():
    src = pathlib.Path(sys.argv[1] if len(sys.argv) > 1 else 'tierd-ui/src')
    bad = 0
    for path in sorted(src.rglob('*.ts*')):
        text = path.read_text()
        for m in re.finditer(r'\bplaceholder=', text):
            expr = expression(text, m.end())
            if not allowed(expr):
                line = text.count('\n', 0, m.start()) + 1
                print(f'::error file={path},line={line},title=Example value in placeholder::placeholder={expr}')
                bad += 1
    if bad:
        print(f'{bad} placeholder(s) must be allowlisted translated instructions '
              f'(see {sys.argv[0]}), not example values.', file=sys.stderr)
        return 1
    print('placeholders OK')
    return 0


if __name__ == '__main__':
    sys.exit(main())
