"""Updates page against a running Vite server.

Run: python tests/browser/updates_page.py http://127.0.0.1:5173
Requires Playwright and Chromium; CHROMIUM_PATH can select a system browser.
API responses are fixtures. The real application and routing run.

Updates install automatically (apt); the page says whether a restart is
needed and why, and restarts only after the operator confirms.
"""
import json
import os
import sys
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright, expect

base = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:5173"
REASON = "SmoothKernel 7.2.10-75-smoothkernel is installed; running 7.2.10-74-smoothkernel"

with sync_playwright() as playwright:
    options = {"headless": True}
    if os.environ.get("CHROMIUM_PATH"):
        options["executable_path"] = os.environ["CHROMIUM_PATH"]
    browser = playwright.chromium.launch(**options)
    for scenario in ("restart_needed", "up_to_date"):
        context = browser.new_context(ignore_https_errors=os.environ.get("ALLOW_INSECURE_TLS") == "1")
        context.add_init_script("localStorage.setItem('sn_session','true'); localStorage.setItem('sn_user','qa');")
        reboots = []

        def respond(route):
            request = route.request
            path = urlparse(request.url).path
            data = []
            if path == "/api/system/reboot" and request.method == "POST":
                reboots.append(path)
                data = {"status": "ok"}
            elif path == "/api/system/reboot":
                data = {"required": True, "reasons": [REASON]} if scenario == "restart_needed" \
                    else {"required": False, "reasons": None}
            elif path in ("/api/locale", "/api/users/me/language"):
                data = {"language": "en", "locale": "en"}
            elif path == "/api/health":
                data = {"status": "ok"}
            route.fulfill(status=200, content_type="application/json", body=json.dumps(data))

        context.route(base + "/api/**", respond)
        page = context.new_page()
        page.goto(base + "/updates?lang=en")
        expect(page.get_by_text("Updates, including security fixes, install automatically.", exact=False)).to_be_visible()
        restart = page.get_by_role("button", name="Restart now", exact=True)
        if scenario == "restart_needed":
            expect(page.get_by_text("A restart is needed to finish installing updates.", exact=True)).to_be_visible()
            expect(page.get_by_text(REASON, exact=True)).to_be_visible()
            page.once("dialog", lambda dialog: dialog.dismiss())
            restart.click()
            page.wait_for_timeout(500)
            assert reboots == [], f"restart without confirmation: {reboots}"
            page.once("dialog", lambda dialog: dialog.accept())
            restart.click()
            expect(page.get_by_text("Restarting. SmoothNAS will be back in a few minutes.", exact=True)).to_be_visible()
            assert reboots == ["/api/system/reboot"], f"expected one restart, got {reboots}"
        else:
            expect(page.get_by_text("No restart is pending.", exact=True)).to_be_visible()
            assert reboots == []
        context.close()
    browser.close()
print("updates page: ok")
