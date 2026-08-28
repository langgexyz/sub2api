#!/usr/bin/env python3
"""检查合并结果是否吞掉了上游引入的符号。

auto-merge 在没有冲突标记的位置可能整段采用本地版本，从而静默丢弃上游新增的
结构体字段、provider 注册和函数。这类丢失不产生冲突，也不一定破坏编译，因此
需要独立校验。

判据：上游相对 merge-base 新增的顶层符号与结构体字段，在合并结果中必须存在。

用法：
    upstream-merge-doctor.py --merge-base <sha> --upstream <ref> [--path <前缀>]

退出码 0 表示未发现丢失，1 表示存在丢失，2 表示调用错误。
"""

from __future__ import annotations

import argparse
import re
import subprocess
import sys

# 顶层函数、方法、类型声明
SYMBOL_RE = re.compile(
    r"^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)|^type\s+([A-Za-z_]\w*)",
    re.MULTILINE,
)
# 结构体字段声明：缩进 + 导出标识符 + 类型。覆盖带 tag 与不带 tag 两种写法，
# 因此不能要求行尾必须是反引号。
FIELD_RE = re.compile(
    r"^[ \t]+([A-Z]\w*)\s+[\w\*\[\]\.]+.*$", re.MULTILINE
)
# wire provider set 条目：单独成行的 `Identifier,`。这类条目的定义在别的文件里，
# 因此必须按「整行」而非「名字出现过」来判定，否则删掉注册行也检不出。
PROVIDER_RE = re.compile(r"^\s*([A-Z]\w*),\s*$", re.MULTILINE)


def git(*args: str) -> str:
    result = subprocess.run(
        ["git", *args], capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        return ""
    return result.stdout


def changed_files(merge_base: str, upstream: str, path: str) -> list[str]:
    """上游相对 merge-base 改过的文件，加上被本地删改的上游文件。

    只看 upstream..merge-base 的 diff 会漏掉一类：上游本次没动、但本地
    （例如一次 revert）把内容删掉的文件。因此并上 merge-base..HEAD 的
    diff，凡两侧任一动过的都纳入扫描。
    """
    upstream_changed = git(
        "diff", "--name-only", f"{merge_base}..{upstream}", "--", path
    ).splitlines()
    locally_changed = git(
        "diff", "--name-only", f"{merge_base}..HEAD", "--", path
    ).splitlines()
    merged = set(upstream_changed) | set(locally_changed)
    return sorted(
        f for f in merged if f.endswith(".go") and "_test.go" not in f
    )


def symbols(text: str) -> set[str]:
    found: set[str] = set()
    for match in SYMBOL_RE.finditer(text):
        found.add(match.group(1) or match.group(2))
    found.update(match.group(1) for match in FIELD_RE.finditer(text))
    found.discard(None)
    return found


def providers(text: str) -> set[str]:
    """wire provider set 里单独成行的注册条目。"""
    return {match.group(1) for match in PROVIDER_RE.finditer(text)}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--merge-base", required=True)
    parser.add_argument("--upstream", required=True)
    parser.add_argument("--path", default="backend/")
    args = parser.parse_args()

    files = changed_files(args.merge_base, args.upstream, args.path)
    if not files:
        print("error: no upstream-changed Go files found; check refs and --path")
        return 2

    missing_total = 0
    for path in files:
        base_text = git("show", f"{args.merge_base}:{path}")
        upstream_text = git("show", f"{args.upstream}:{path}")
        if not upstream_text:
            continue
        try:
            with open(path, encoding="utf-8") as handle:
                merged_text = handle.read()
        except FileNotFoundError:
            print(f"error: {path}: upstream has this file but merge result does not")
            missing_total += 1
            continue

        # 判据：上游当前拥有的符号，合并结果里都应该还在。
        #
        # 不能只查「上游新增」（upstream - base）：本次同步要修复的正是一次
        # revert，它删掉的符号在 base 里同样存在，只按新增判据会全部漏报。
        # 反过来，fork 有意替换掉的上游符号会被报出来，需要人工确认——
        # 这类误报可接受，漏报不可接受。
        base_providers = providers(base_text)
        upstream_providers = providers(upstream_text)
        merged_providers = providers(merged_text)
        missing_providers = (upstream_providers - merged_providers) & (
            upstream_providers | base_providers
        )

        upstream_symbols = symbols(upstream_text) - upstream_providers
        missing_symbols = {
            name for name in upstream_symbols if name not in merged_text
        }

        missing = sorted(missing_providers | missing_symbols)
        if missing:
            missing_total += len(missing)
            print(f"error: {path}")
            for name in missing:
                print(f"  missing upstream symbol: {name}")

    if missing_total:
        print(f"\nerror: {missing_total} upstream symbol(s) missing from merge result")
        return 1
    print(f"ok: checked {len(files)} upstream-changed files, no missing symbols")
    return 0


if __name__ == "__main__":
    sys.exit(main())
