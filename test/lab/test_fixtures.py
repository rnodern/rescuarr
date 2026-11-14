import json
import unittest
from pathlib import Path

FIXTURE = Path(__file__).with_name("fixtures") / "reads.json"


class ReadFixturesTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fixture = json.loads(FIXTURE.read_text())
        cls.cases = {case["id"]: case for case in cls.fixture["cases"]}

    def test_source_shaped_examples_are_labelled_and_contain_no_credentials(self):
        self.assertEqual(self.fixture["evidenceClass"], "synthetic")
        self.assertEqual(len(self.cases), len(self.fixture["cases"]))
        for case in self.cases.values():
            self.assertEqual(case["request"]["method"], "GET")
            self.assertTrue(case["request"]["path"].startswith("/api"))
            self.assertNotIn("apikey", json.dumps(case).lower())
            self.assertNotIn("://", json.dumps(case))

    def test_pack_has_two_exact_history_joins(self):
        rows = self.cases["sonarr-pack-two-episodes"]["response"]["body"]["records"]
        history = self.cases["sonarr-pack-history"]["response"]["body"]["records"]

        def identity(row):
            return (row["downloadId"], row["seriesId"], row["episodeId"])

        self.assertEqual(len({row["id"] for row in rows}), 2)
        self.assertEqual(len({row["downloadId"] for row in rows}), 1)
        self.assertEqual({identity(row) for row in rows}, {identity(row) for row in history})


if __name__ == "__main__":
    unittest.main()
