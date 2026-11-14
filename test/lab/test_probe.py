import configparser
import tempfile
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import patch

import probe


class SabProbeTest(unittest.TestCase):
    def test_reads_configobj_preamble_and_authenticates_queue_request(self):
        key = "a" * 32
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "sabnzbd.ini"
            config.write_text(
                f"__encoding__ = utf-8\n__version__ = 19\n[misc]\napi_key = \"{key}\"\n[servers]\n[[example]]\n",
                encoding="utf-8",
            )
            with self.assertRaises(configparser.MissingSectionHeaderError):
                configparser.ConfigParser().read(config)
            parsed = probe.sab_key(config)

        with patch.object(probe, "sab_key", return_value=parsed), patch.object(
            probe, "get_json", side_effect=[{"version": "5.1.3"}, {"queue": {"slots": []}}]
        ) as read:
            result = probe.sab_snapshot()
        self.assertEqual(result, {"product": "sabnzbd", "version": "5.1.3", "queueTotal": 0})
        self.assertEqual(read.call_args_list[1].args[2]["apikey"], key)
        self.assertNotIn(key, repr(result))

    def test_deadline_reports_http_status_and_omits_request_details(self):
        error = urllib.error.HTTPError(
            "http://sab:8080/api?apikey=synthetic", 403, "private response", {}, None
        )
        with patch.object(probe, "get_json", side_effect=error):
            with self.assertRaises(probe.ProbeFailure) as failure:
                probe.wait_for(lambda: probe.sab_read("queue", {}), timeout=0)
        self.assertEqual(failure.exception.details, {"phase": "queue", "category": "httpError", "httpStatus": 403})
        self.assertNotIn("synthetic", str(failure.exception))
        self.assertNotIn("private response", str(failure.exception))

    def test_invalid_key_details_are_safe(self):
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory) / "sabnzbd.ini"
            config.write_text("__version__ = 19\n[misc]\napi_key = invalid-value\n", encoding="utf-8")
            with self.assertRaises(probe.ProbeFailure) as failure:
                probe.sab_key(config)
        self.assertEqual(failure.exception.details, {"phase": "config", "category": "missingOrInvalidKey"})
        self.assertNotIn("invalid-value", str(failure.exception))
