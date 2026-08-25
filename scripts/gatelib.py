#!/usr/bin/env python3
"""Helpers shared by the gates, kept in ONE place.

A helper copied rather than imported diverges, and the copies diverge in the direction that
matters: the second author writes the simple version. scripts/floor.py carried a
five-word comment stripper that only blanked FULL-LINE comments, beside this
quote-aware one — so a check reading that view saw a trailing comment as content, which is
the exact defect the stripper exists to prevent.

Not a gate. scripts/floor.py excludes it by name, with that reason recorded there.
"""


def blank_comment_body(line):
    """Blank a trailing YAML comment's BODY, respecting quotes, keeping the '#'.

    One stripper, two views, chosen per check — reading one view for two purposes is how a
    gate goes blind:

      raw       when the thing being looked for IS an annotation. The `# v7.0.1` beside a
                SHA is not decoration, it is what Renovate rewrites, so the version-comment
                check reads the raw line.
      blanked   when a comment must not be able to satisfy a check looking for content. A
                commented-out `uses:` is not an action reference, and a customManager's
                regex must match a real pin rather than prose mentioning one.

    The body is blanked rather than the line deleted so the '#' survives and offsets stay
    put, which keeps reported line numbers true. Naive splitting on '#' would also cut a
    URL fragment or a quoted value in half, so quotes are respected.
    """
    out, quote = [], None
    i = 0
    while i < len(line):
        c = line[i]
        if quote:
            out.append(c)
            if c == "\\" and i + 1 < len(line):
                out.append(line[i + 1])
                i += 2
                continue
            if c == quote:
                quote = None
        elif c in "\"'":
            quote = c
            out.append(c)
        elif c == "#" and (i == 0 or line[i - 1] in " \t"):
            break
        else:
            out.append(c)
        i += 1
    return "".join(out) + ("#" if quote is None and "#" in line[len("".join(out)):] else "")
