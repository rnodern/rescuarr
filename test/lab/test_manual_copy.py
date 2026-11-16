import unittest
from pathlib import Path
from types import SimpleNamespace

from manual_import_browser import (
    COMMAND, PRESERVATION_NOTICE, REPROCESS, SONARR_CLEANUP_NOTICE, copy_review, prove_copy_command, prove_reprocess,
)
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

    def test_review_requires_correct_cleanup_notice_for_each_profile(self):
        def review(notice):
            forms = "".join(
                '<form method="post" action="/manual-review">'
                f'<input type="hidden" name="decision" value="{decision}">'
                '<input type="hidden" name="token" value="case-token">'
                '<input type="hidden" name="csrf" value="csrf-token"></form>'
                for decision in ("decline", "approve")
            )
            return SimpleNamespace(get=lambda _: (
                f"<h1>Manual Review</h1>Synthetic.S01E01 {self.path.parent.name} "
                f"{self.path.name} Approve Copy import <p>{notice}</p>{forms}"
            ))

        encoded_notice = SONARR_CLEANUP_NOTICE.replace("’", "&#8217;").replace("Its completed", "Its\ncompleted")
        for notice, sonarr_cleanup in ((PRESERVATION_NOTICE, False), (encoded_notice, True)):
            with self.subTest(sonarr_cleanup=sonarr_cleanup):
                decline, approval = copy_review(review(notice), "Synthetic.S01E01", self.path.parent, self.path,
                                                sonarr_cleanup=sonarr_cleanup)
                self.assertEqual(decline["token"], approval["token"])
                with self.assertRaisesRegex(LabFailure, "manualCopyReviewEvidenceIncomplete"):
                    copy_review(review(notice), "Synthetic.S01E01", self.path.parent, self.path,
                                sonarr_cleanup=not sonarr_cleanup)

    def test_reject_queue_mutation_and_duplicate_or_changed_copy(self):
        unexpected = {"method": "DELETE", "route": "/api/v3/queue/21"}
        for writes in ([self.copy, unexpected], [self.copy, self.copy],
                       [{**self.copy, "request": {**self.copy["request"], "importMode": "move"}}],
                       [{**self.copy, "request": {**self.copy["request"], "files": [{**self.file, "episodeIds": [7, 8]}]}}]):
            with self.subTest(writes=writes), self.assertRaises(LabFailure):
                prove_copy_command(writes, "fixture-job", self.path, self.episode)
