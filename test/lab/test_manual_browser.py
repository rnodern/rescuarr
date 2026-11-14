import unittest
from unittest.mock import patch

from manual_browser import OwnerBrowser, PostForms


class ManualBrowserFormsTest(unittest.TestCase):
    def test_approve_and_decline_use_their_own_tokens_on_the_same_route(self):
        forms = PostForms("""
            <form method="post" action="/manual-review">
              <input name="decision" type="hidden" value="approve">
              <input name="csrf" type="hidden" value="owner-token">
              <input name="token" type="hidden" value="approval">
            </form>
            <form action="/manual-review" method="post">
              <input type="hidden" name="decision" value="decline">
              <input type="hidden" name="csrf" value="owner-token">
              <input type="hidden" name="token" value="selection">
            </form>
        """)
        self.assertEqual(forms.form("/manual-review", decision="approve")["token"], "approval")
        self.assertEqual(forms.form("/manual-review", decision="decline")["token"], "selection")

    def test_connection_check_form_accepts_saved_base32_identity(self):
        browser = OwnerBrowser()
        identity = "YKEMMILS4F65CTPY4PDOCZLWEE"
        route = f"/connections/{identity}/check"
        connection_page = f"""
            <form method="post" action="{route}"><input type="hidden" name="csrf" value="owner"></form>
            <form method="post" action="/connections"><input type="hidden" name="csrf" value="owner"></form>
        """
        class DisposableArr:
            origin = "http://127.0.0.1:18989"
            headers = {"X-Api-Key": "fixture-key"}

        with patch.object(browser, "get", return_value=connection_page), patch.object(browser, "post") as post:
            self.assertEqual(browser.connect(DisposableArr()), identity)
        self.assertEqual(post.call_args.args, (route, "/connections", {"csrf": "owner"}))
