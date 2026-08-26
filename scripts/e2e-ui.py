#!/usr/bin/env python3
"""e2e-ui.py — 管理端浏览器级冒烟（HARDENING.md round 189 探针固化）。

用法:
  BASE=http://127.0.0.1:8000 ADMIN_USER=root ADMIN_PASS=... python3 scripts/e2e-ui.py

前置: pip install playwright && playwright install chromium
依赖缺失时显式 SKIP（退出码 3），绝不假通过。

覆盖链路: 登录表单 → dashboard 重定向 → 导航栏渲染 → 会话持久化
（reload 不回登录页）→ 密钥管理页渲染。全部只读，无数据变更。

退出码: 0=PASS  1=FAIL  3=依赖缺失(SKIP)
"""
import asyncio
import os
import sys

BASE = os.environ.get("BASE", "http://127.0.0.1:8000")
ADMIN_USER = os.environ.get("ADMIN_USER", "root")
ADMIN_PASS = os.environ.get("ADMIN_PASS") or sys.exit("need ADMIN_PASS")

try:
    from playwright.async_api import async_playwright
except ImportError:
    print("E2E-UI: SKIP — playwright 未安装 (pip install playwright && playwright install chromium)")
    sys.exit(3)


async def main() -> int:
    failures = []
    async with async_playwright() as p:
        browser = await p.chromium.launch(headless=True)
        page = await browser.new_page(viewport={"width": 1440, "height": 900})
        page_errors = []
        page.on("pageerror", lambda e: page_errors.append(str(e)))

        # 1) 登录链路
        await page.goto(BASE + "/", wait_until="networkidle")
        await page.locator('input[type="text"], input[name="username"]').first.fill(ADMIN_USER)
        await page.locator('input[type="password"]').first.fill(ADMIN_PASS)
        await page.locator('input[type="password"]').first.press("Enter")
        try:
            await page.wait_for_url("**/dashboard", timeout=15000)
        except Exception:
            failures.append(f"login did not reach /dashboard (url={page.url})")
            await browser.close()
            print("E2E-UI: FAIL —", "; ".join(failures))
            return 1

        # 2) 导航完整性：核心入口标题必须出现在渲染后的 shell 中
        await page.wait_for_timeout(1500)
        body = await page.inner_text("body")
        for nav in ("仪表盘", "账号", "密钥", "模型", "请求审计"):
            if nav not in body:
                failures.append(f"nav item missing: {nav}")

        # 3) 会话持久化：reload 后仍持 /dashboard（无登录循环）
        await page.reload(wait_until="networkidle")
        if "/dashboard" not in page.url:
            failures.append(f"session lost after reload (url={page.url})")

        # 4) 密钥页渲染 + 无前端崩溃
        await page.click("text=密钥")
        await page.wait_for_timeout(1500)
        if "/client-keys" not in page.url:
            failures.append(f"keys page did not open (url={page.url})")
        if page_errors:
            failures.append(f"pageerror(s): {page_errors[:3]}")

        await browser.close()

    if failures:
        print("E2E-UI: FAIL —", "; ".join(failures))
        return 1
    print("E2E-UI: PASS (login → dashboard → nav → session → keys)")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
