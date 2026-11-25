#!/usr/bin/env python3
"""从本机 google-colab-cli 所在桌面 Keyring 导出 OAuth2 Token 到 0600 权限的 JSON 文件。

用法（建议直接用安装了 google-colab-cli 的 Python 运行）：
  ~/.local/share/uv/tools/google-colab-cli/bin/python scripts/export_local_token.py -o ./google_token.json
"""

import argparse
import json
import os
import sys
from pathlib import Path


def main() -> int:
    parser = argparse.ArgumentParser(description="导出本机 google-colab-cli Keyring 中的 OAuth2 Token")
    parser.add_argument("-o", "--output", default="./token.json", help="输出文件路径（权限自动设为 0600）")
    args = parser.parse_args()

    try:
        import keyring
    except ImportError:
        print("错误：当前 Python 环境未安装 keyring，请改用对应 CLI 环境的 Python 解释器运行。", file=sys.stderr)
        return 1

    raw = keyring.get_password("google-colab-cli", "oauth_token")
    if not raw:
        print("未在系统 Keyring 中找到凭据，请先在本机完成登录或直接在服务器运行 `./comfelle auth`。", file=sys.stderr)
        return 1

    data = json.loads(raw)
    out = Path(args.output)
    out.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(str(out), os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2)
    os.chmod(out, 0o600)
    print(f"已导出 OAuth2 Token 到 {out.resolve()}（权限 0600），请安全上传到服务器并妥善保管。")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
