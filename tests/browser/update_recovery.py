"""Update-page recovery regression against a running Vite server.

Run: python tests/browser/update_recovery.py http://127.0.0.1:5173
Requires Playwright and Chromium; CHROMIUM_PATH can select a system browser.
API responses are fixtures. The real application, routing and timers run.
"""
import json
import os
import sys
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

base = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:5173"
with sync_playwright() as playwright:
    options = {"headless": True}
    if os.environ.get("CHROMIUM_PATH"):
        options["executable_path"] = os.environ["CHROMIUM_PATH"]
    browser = playwright.chromium.launch(**options)
    for scenario in ("system_failed", "system_active", "package_failed", "package_active"):
        context = browser.new_context(ignore_https_errors=os.environ.get("ALLOW_INSECURE_TLS") == "1")
        context.add_init_script("localStorage.setItem('sn_session','true'); localStorage.setItem('sn_user','qa');")
        counts = {"system": 0, "package": 0}

        def respond(route):
            path = urlparse(route.request.url).path
            data = []
            if path == "/api/system/update/channel":
                data = {"channel": "testing"}
            elif path == "/api/system/update/check":
                data = {"current_version": "qa", "channel": "testing", "available": False}
            elif path == "/api/system/update/progress":
                counts["system"] += 1
                data = {"stage": "idle"}
                if scenario == "system_failed":
                    data = {"stage": "failed", "error": "QA checksum mismatch"}
                elif scenario == "system_active":
                    data = {"stage": "downloading"} if counts["system"] <= 2 else {"stage": "failed", "error": "QA interrupted download"}
            elif path == "/api/system/debian/progress":
                counts["package"] += 1
                data = {"stage": "idle"}
                if scenario == "package_failed":
                    data = {"stage": "failed", "error": "QA package failure"}
                elif scenario == "package_active":
                    data = {"stage": "installing"} if counts["package"] <= 2 else {"stage": "complete"}
            elif path in ("/api/locale", "/api/users/me/language"):
                data = {"language": "en", "locale": "en"}
            elif path == "/api/health":
                data = {"status": "ok"}
            route.fulfill(status=200, content_type="application/json", body=json.dumps(data))

        context.route(base + "/api/**", respond)
        page = context.new_page()
        page.goto(base + "/updates?lang=en")
        upload = page.locator('input[type="file"]')
        expect(upload).to_be_visible()
        if scenario == "system_failed":
            expect(page.get_by_text("QA checksum mismatch", exact=True)).to_be_visible()
            page.get_by_role("button", name="Refresh", exact=True).click()
            expect(page.get_by_text("QA checksum mismatch", exact=True)).to_be_visible()
            page.reload()
            expect(page.get_by_text("QA checksum mismatch", exact=True)).to_be_visible()
        elif scenario == "system_active":
            expect(upload).to_be_disabled()
            expect(page.get_by_text("QA interrupted download", exact=True)).to_be_visible(timeout=7000)
            expect(upload).to_be_enabled()
            assert counts["system"] >= 3, "system progress polling did not resume"
        elif scenario == "package_failed":
            expect(page.get_by_text("QA package failure", exact=True)).to_be_visible()
        else:
            button = page.get_by_role("button", name="installing", exact=True)
            expect(button).to_be_disabled()
            expect(button).not_to_be_visible(timeout=7000)
            assert counts["package"] >= 3, "package polling did not resume"
        print("PASS", scenario, flush=True)
        context.close()
    browser.close()
