"""Updates page against a running Vite server.

Run: python tests/browser/updates_page.py http://127.0.0.1:5173
Requires Playwright and Chromium; CHROMIUM_PATH can select a system browser.
API responses are fixtures. The real application and routing run.

The page lists available updates, checks for and installs them on request
while following the run's log, shows a failed run's errors (an unreachable
archive must not be silent), says when a failed install has since been
superseded by successful unattended upgrades and when an install succeeded
although a package source couldn't be refreshed, says whether a restart is
needed and why, and restarts only after the operator confirms.
"""
import json
import os
import sys
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import sync_playwright, expect

base = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:5173"
REASON = "smoothfs 0.3.2 is installed; running 0.3.1"
RESOLVE_ERR = "Err:5 https://updates.rakuensoftware.com/public common InRelease: Temporary failure resolving 'updates.rakuensoftware.com'"
BCACHEFS_ERR = "Err:4 https://apt.bcachefs.org/trixie bcachefs-tools-release InRelease: Could not connect to apt.bcachefs.org:443 (5.161.236.196). - connect (111: Connection refused)"
LOG = "== smoothnas-update start install 2026-10-29T18:10:00Z\nSetting up smoothfs-dkms (0.3.2-1) ...\n"


def status(scenario, state):
    no_reboot = {"required": False, "reasons": None}
    if scenario == "restart_needed":
        return {"running": False, "available": [], "reboot": {"required": True, "reasons": [REASON]},
                "last_run": {"action": "install", "started_at": "t0", "finished_at": "t1", "exit_code": 0, "errors": []}}
    if scenario == "up_to_date":
        # A check started from the page: the run is active but its log still
        # holds the previous (finished install) run, as just after starting.
        if state["checks"]:
            return {"running": True, "available": [], "reboot": no_reboot,
                    "last_run": {"action": "install", "started_at": "t0", "finished_at": "t1", "exit_code": 0, "errors": []}}
        return {"running": False, "available": [], "reboot": no_reboot,
                "last_run": {"action": "check", "started_at": "t0", "finished_at": "2026-10-29T18:00:00Z", "exit_code": 0, "errors": []}}
    # The WTR Max on 2026-11-02: an install started here failed on Nov 1
    # because apt.bcachefs.org was down; unattended upgrades have succeeded
    # since. That failure is history, not an error.
    if scenario == "install_failed_superseded":
        return {"running": False, "available": [], "reboot": no_reboot,
                "last_run": {"action": "install", "started_at": "2026-11-01T15:36:21Z", "finished_at": "2026-11-01T15:36:28Z",
                             "exit_code": 100, "errors": [BCACHEFS_ERR], "superseded_at": "2026-11-02T13:38:27Z"}}
    # Negative control: the same failure, nothing has succeeded since.
    if scenario == "install_failed":
        return {"running": False, "available": [], "reboot": no_reboot,
                "last_run": {"action": "install", "started_at": "2026-11-01T15:36:21Z", "finished_at": "2026-11-01T15:36:28Z",
                             "exit_code": 100, "errors": [BCACHEFS_ERR]}}
    # A source was down, but the install went ahead and succeeded.
    if scenario == "installed_refresh_failed":
        return {"running": False, "available": [], "reboot": no_reboot,
                "last_run": {"action": "install", "started_at": "t0", "finished_at": "2026-11-02T15:00:00Z",
                             "exit_code": 0, "refresh_failed": True, "errors": [BCACHEFS_ERR]}}
    if scenario == "check_failed":
        return {"running": False, "available": [], "reboot": no_reboot,
                "last_run": {"action": "check", "started_at": "t0", "finished_at": "t1", "exit_code": 100, "errors": [RESOLVE_ERR]}}
    # available -> install
    if state["installs"]:
        return {"running": True, "available": [], "reboot": no_reboot,
                "last_run": {"action": "install", "started_at": "t2", "errors": []}}
    return {"running": False, "reboot": no_reboot, "last_run": None,
            "available": [{"name": "smoothfs-dkms", "installed": "0.3.1-1", "available": "0.3.2-1"}]}


with sync_playwright() as playwright:
    options = {"headless": True}
    if os.environ.get("CHROMIUM_PATH"):
        options["executable_path"] = os.environ["CHROMIUM_PATH"]
    browser = playwright.chromium.launch(**options)
    for scenario in ("restart_needed", "up_to_date", "check_failed", "install_failed_superseded",
                     "install_failed", "installed_refresh_failed", "available"):
        context = browser.new_context(ignore_https_errors=os.environ.get("ALLOW_INSECURE_TLS") == "1")
        context.add_init_script("localStorage.setItem('sn_session','true'); localStorage.setItem('sn_user','qa');")
        reboots = []
        state = {"installs": 0, "checks": 0}

        def respond(route):
            request = route.request
            url = urlparse(request.url)
            path = url.path
            data = []
            if path == "/api/system/reboot" and request.method == "POST":
                reboots.append(path)
                data = {"status": "ok"}
            elif path == "/api/system/updates":
                data = status(scenario, state)
            elif path == "/api/system/updates/check" and request.method == "POST":
                state["checks"] += 1
                data = {"status": "started"}
            elif path == "/api/system/updates/install" and request.method == "POST":
                state["installs"] += 1
                data = {"status": "started"}
            elif path == "/api/system/updates/log":
                offset = int(parse_qs(url.query).get("offset", ["0"])[0])
                text = LOG if state["installs"] and offset == 0 else ""
                data = {"text": text, "next_offset": len(LOG) if state["installs"] else 0}
            elif path in ("/api/locale", "/api/users/me/language"):
                data = {"language": "en", "locale": "en"}
            elif path == "/api/health":
                data = {"status": "ok"}
            elif path == "/api/network/pending":
                data = None
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
        elif scenario == "up_to_date":
            expect(page.get_by_text("SmoothNAS is up to date.", exact=False)).to_be_visible()
            expect(page.get_by_text("No restart is pending.", exact=True)).to_be_visible()
            assert reboots == []
            page.get_by_role("button", name="Check for updates", exact=True).click()
            expect(page.get_by_text("Checking for updates…", exact=True)).to_be_visible()
            assert page.get_by_text("Installing updates…", exact=False).count() == 0, "a check was labelled as an install"
        elif scenario == "install_failed_superseded":
            expect(page.get_by_text("SmoothNAS is up to date.", exact=False)).to_be_visible()
            expect(page.get_by_text("automatic updates have installed successfully since (2026-11-02T13:38:27Z)", exact=False)).to_be_visible()
            assert page.get_by_text("Installing updates failed", exact=False).count() == 0, "a superseded failure is shown as current"
            assert page.locator(".error-msg").count() == 0, "a superseded failure is shown as an error"
        elif scenario == "install_failed":
            expect(page.get_by_text("Installing updates failed (2026-11-01T15:36:28Z).", exact=True)).to_be_visible()
            expect(page.get_by_text(BCACHEFS_ERR, exact=True)).to_be_visible()
            assert page.get_by_text("automatic updates have installed", exact=False).count() == 0
        elif scenario == "installed_refresh_failed":
            expect(page.get_by_text("Updates were installed (2026-11-02T15:00:00Z), but these package sources could not be refreshed:", exact=True)).to_be_visible()
            expect(page.get_by_text(BCACHEFS_ERR, exact=True)).to_be_visible()
            assert page.get_by_text("Installing updates failed", exact=False).count() == 0, "a successful install is shown as failed"
        elif scenario == "check_failed":
            expect(page.get_by_text("Checking for updates failed", exact=False)).to_be_visible()
            expect(page.get_by_text(RESOLVE_ERR, exact=True)).to_be_visible()
        else:
            expect(page.get_by_text("1 update(s) available", exact=True)).to_be_visible()
            expect(page.get_by_role("cell", name="0.3.2-1", exact=True)).to_be_visible()
            page.get_by_role("button", name="Install updates", exact=True).click()
            expect(page.get_by_text("Installing updates…", exact=False)).to_be_visible()
            expect(page.get_by_text("Setting up smoothfs-dkms (0.3.2-1) ...", exact=False)).to_be_visible()
            assert state["installs"] == 1, f"expected one install request, got {state['installs']}"
            expect(page.get_by_role("button", name="Install updates", exact=True)).to_be_disabled()
        context.close()
    browser.close()
print("updates page: ok")
