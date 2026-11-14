import unittest
from pathlib import Path

from manual_import_browser import COMMAND, REPROCESS, prove_copy_command, prove_reprocess
from scenarios import LabFailure


class ManualCopyEvidenceTest(unittest.TestCase):
    def setUp(self):
        self.path = Path("/downloads/complete/Synthetic/Synthetic.mkv")
        self.episode = {"id": 7, "seriesId": 3}
        self.file = {"path": str(self.path), "downloadId": "fixture-job", "seriesId": 3, "episodeIds": [7]}
        self.reprocess = {"method": "POST", "route": REPROCESS, "request": [self.file]}
        self.copy = {"method": "POST", "route": COMMAND, "request": {
            "name": "ManualImport", "importMode": "copy", "files": [self.file],
        }, "commandId": 9}

    def test_one_native_copy_and_exact_candidate_reprocess(self):
        self.assertEqual(prove_reprocess([self.reprocess], "fixture-job", self.path, self.episode), 1)
        self.assertEqual(prove_copy_command([self.reprocess, self.copy], "fixture-job", self.path, self.episode), 9)

    def test_reject_queue_mutation_and_duplicate_or_changed_copy(self):
        unexpected = {"method": "DELETE", "route": "/api/v3/queue/21"}
        for writes in ([self.copy, unexpected], [self.copy, self.copy],
                       [{**self.copy, "request": {**self.copy["request"], "importMode": "move"}}],
                       [{**self.copy, "request": {**self.copy["request"], "files": [{**self.file, "episodeIds": [7, 8]}]}}]):
            with self.subTest(writes=writes), self.assertRaises(LabFailure):
                prove_copy_command(writes, "fixture-job", self.path, self.episode)
