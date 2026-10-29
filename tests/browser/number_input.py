"""Numeric fields can be cleared and retyped, against a running Vite server.

Run: python tests/browser/number_input.py http://127.0.0.1:5173
Requires Playwright and Chromium; CHROMIUM_PATH can select a system browser.

A field bound to Number(e.target.value) turned an emptied field into 0 on
every keystroke, so the tier rank read "07" after clearing it and typing 7.
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
    context = browser.new_context()
    context.add_init_script("localStorage.setItem('sn_session','true'); localStorage.setItem('sn_user','qa');")
    created = []

    def respond(route):
        request = route.request
        path = urlparse(request.url).path
        data = []
        if path == "/api/tiers" and request.method == "POST":
            created.append(json.loads(request.post_data or "{}"))
            data = {"status": "ok"}
        elif path in ("/api/locale", "/api/users/me/language"):
            data = {"language": "en", "locale": "en"}
        elif path == "/api/network/pending":
            data = None
        route.fulfill(status=200, content_type="application/json", body=json.dumps(data))

    context.route(base + "/api/**", respond)
    page = context.new_page()
    page.goto(base + "/tiers?lang=en")
    page.locator(".create-card input").first.fill("media")
    page.get_by_role("button", name="Advanced", exact=False).click()
    rank = page.locator(".create-card input[type=number]").nth(1)
    rank.click()
    page.keyboard.press("End")
    page.keyboard.press("Backspace")
    expect(rank).to_have_value("")
    page.keyboard.type("7")
    expect(rank).to_have_value("7")
    page.get_by_role("button", name="Create", exact=False).last.click()
    page.wait_for_timeout(500)
    assert created, "no create request"
    ranks = [t["rank"] for t in created[0].get("tiers", [])]
    assert ranks == [1, 7, 3], f"expected ranks [1, 7, 3], got {ranks}"
    browser.close()
print("number input: ok")
