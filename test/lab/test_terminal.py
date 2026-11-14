import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

from scenarios import CATEGORY, INDEXER_NAME, LabFailure, sab_job, new_failure_records, terminal_failed_job


class TerminalFailureTest(unittest.TestCase):
    def test_exact_terminal_job_requires_failed_history_and_absence_from_queue(self):
        download_id = "SABnzbd_nzo_original"
        title = "Synthetic release"
        product = "sonarr"
        with tempfile.TemporaryDirectory() as directory:
            downloads = Path(directory).resolve()
            incomplete = downloads / "incomplete" / "synthetic"
            complete = downloads / "complete" / "synthetic"
            incomplete.mkdir(parents=True)
            complete.mkdir(parents=True)
            failed = {"nzo_id": download_id, "name": title, "category": CATEGORY[product],
                      "status": "Failed", "loaded": False, "archive": False,
                      "fail_message": "Download failed - Not on your server(s)",
                      "path": str(incomplete), "storage": str(complete)}
            unrelated = {**failed, "nzo_id": "SABnzbd_nzo_unrelated"}
            history = [unrelated, {**failed, "status": "Extracting"}]
            queue = [unrelated]

            def read(mode, **query):
                self.assertEqual(query["nzo_ids"], download_id)
                slots = queue if mode == "queue" else history
                return {mode: {"slots": slots}}

            with patch("scenarios.sab_api", side_effect=read), patch("scenarios.DOWNLOADS", downloads):
                self.assertIsNone(terminal_failed_job(download_id, title, product))
                history[1] = {**failed, "loaded": True}
                self.assertIsNone(terminal_failed_job(download_id, title, product))
                history[1] = failed
                queue.append(failed)
                self.assertIsNone(terminal_failed_job(download_id, title, product))
                queue.pop()
                terminal = terminal_failed_job(download_id, title, product)
                self.assertEqual(terminal["client"], sab_job(download_id))
                self.assertEqual(terminal["client"]["queue"], [])
                self.assertEqual([row["nzo_id"] for row in terminal["client"]["history"]], [download_id])
                self.assertEqual(terminal["outputs"], [
                    {"path": str(complete), "exists": True, "media": []},
                    {"path": str(incomplete), "exists": True, "media": []},
                ])
                (complete / "video.mkv").write_bytes(b"media")
                with self.assertRaisesRegex(LabFailure, "failedOriginalHasSalvageableMedia"):
                    terminal_failed_job(download_id, title, product)
                history[1] = {**failed, "category": CATEGORY["radarr"]}
                with self.assertRaisesRegex(LabFailure, "SABFailureIdentityMismatch"):
                    terminal_failed_job(download_id, title, product)
                history.append(failed)
                with self.assertRaisesRegex(LabFailure, "ambiguousSABHistory"):
                    terminal_failed_job(download_id, title, product)

    def test_new_arr_failure_requires_exact_history_and_new_scoped_blocklist(self):
        original = {"downloadId": "original", "sourceTitle": "Original Release", "quality": {"id": 5}}
        for product, media_field, scope, other_scope in (
            ("sonarr", "episodeId", {"seriesId": 8, "episodeIds": [1]}, {"seriesId": 8, "episodeIds": [1, 2]}),
            ("radarr", "movieId", {"movieId": 1}, {"movieId": 2}),
        ):
            with self.subTest(product=product):
                media = {"id": 1, "seriesId": 8}
                failure = {"id": 21, "eventType": "downloadFailed", **original, media_field: 1}
                block = {"id": 31, "sourceTitle": original["sourceTitle"], "quality": original["quality"],
                         "protocol": "usenet", "indexer": INDEXER_NAME, **scope}
                arr = Mock(product=product)
                arr.history.return_value = [
                    {**failure, "id": 20},
                    {**failure, "id": 22, "downloadId": "unrelated"},
                    {**failure, "id": 23, media_field: 2},
                    failure,
                ]
                arr.blocklist.return_value = [{**block, "id": 30}, {**block, **other_scope}]
                self.assertIsNone(new_failure_records(arr, media, original, {30}, {20}))
                arr.blocklist.return_value.append(block)
                evidence = new_failure_records(arr, media, original, {30}, {20})
                self.assertEqual([row["id"] for row in evidence["failureHistory"]], [21])
                self.assertEqual(evidence["blocklist"], [block])
                arr.history.assert_called_with(downloadId="original")
                arr.blocklist.assert_called_with(media)
                arr.blocklist.return_value.append({**block, "id": 32})
                with self.assertRaisesRegex(LabFailure, "rejectProvenanceAmbiguous"):
                    new_failure_records(arr, media, original, {30}, {20})


if __name__ == "__main__":
    unittest.main()
