import hashlib
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

import empty_pack
import probe
from scenarios import LabFailure, completed_fixture_media, run_scenarios
from source import EMPTY_PACK_PHASE, EMPTY_PACK_TARGET, TVDB_ID, releases, search_targets, valid_phase


class EmptyPackLabTest(unittest.TestCase):
    def test_indexer_targets_season_and_episodes_independently(self):
        query = {"t": ["tvsearch"], "tvdbid": [str(TVDB_ID)], "season": ["1"]}
        self.assertEqual(search_targets("sonarr", query), [EMPTY_PACK_TARGET])
        self.assertTrue(valid_phase(EMPTY_PACK_TARGET, EMPTY_PACK_PHASE))
        self.assertFalse(valid_phase(EMPTY_PACK_TARGET, "original"))
        self.assertFalse(valid_phase("episode1", EMPTY_PACK_PHASE))
        self.assertIn(".S01.720p.", releases(EMPTY_PACK_TARGET, EMPTY_PACK_PHASE))
        self.assertEqual(search_targets("sonarr", {**query, "ep": ["1"]}), ["episode1"])

    def test_completed_output_accepts_sab_file_or_directory_with_exact_clip(self):
        with tempfile.TemporaryDirectory() as temp, patch("scenarios.DOWNLOADS", Path(temp).resolve()):
            category = Path(temp).resolve() / "complete" / "sonarr-lab"
            title = releases(EMPTY_PACK_TARGET, EMPTY_PACK_PHASE)
            folder = category / title
            folder.mkdir(parents=True)
            clip = folder / "renamed-by-sab.mkv"
            contents = b"fixture clip"
            clip.write_bytes(contents)
            expected = {"bytes": len(contents), "sha256": hashlib.sha256(contents).hexdigest()}
            for storage, kind in ((folder, "directory"), (clip, "file")):
                evidence = {}
                self.assertEqual(completed_fixture_media(str(storage), title, expected, evidence), (folder, clip))
                self.assertEqual(evidence["completedFixtureLayout"]["storageKind"], kind)
            wrong_hash = {**expected, "sha256": "0" * 64}
            evidence = {}
            with self.assertRaisesRegex(LabFailure, "unexpectedSyntheticPackLayout"):
                completed_fixture_media(str(clip), title, wrong_hash, evidence)
            self.assertFalse(evidence["completedFixtureLayout"]["contentMatch"])
            self.assertNotIn(str(folder), str(evidence))
            spare = folder / "sidecar.txt"
            spare.write_bytes(contents)
            with self.assertRaisesRegex(LabFailure, "unexpectedSyntheticPackLayout"):
                completed_fixture_media(str(folder), title, expected, {})
            spare.unlink()
            outside = category / "outside.mkv"
            outside.write_bytes(contents)
            clip.unlink()
            clip.symlink_to(outside)
            with self.assertRaisesRegex(LabFailure, "unexpectedSyntheticPackLayout"):
                completed_fixture_media(str(folder), title, expected, {})

    def test_exact_pending_aliases_need_matching_warning_and_path(self):
        with tempfile.TemporaryDirectory() as temp, patch("scenarios.DOWNLOADS", Path(temp).resolve()):
            output = Path(temp).resolve() / "complete" / "pack"
            line = empty_pack.EMPTY_WARNING + str(output)
            row = {"id": 4, "seriesId": 1, "episodeId": 2, "protocol": "usenet",
                   "status": "completed", "trackedDownloadState": "importPending",
                   "outputPath": str(output), "statusMessages": [{"messages": [line]}]}
            second = {**row, "id": 5, "episodeId": 3}
            arr = Mock()
            arr.queue.return_value = [second, row]
            self.assertEqual(empty_pack.no_files_queue(arr, "one-pack", output, 1, [2, 3]), [row, second])
            arr.queue.return_value = [row]
            self.assertIsNone(empty_pack.no_files_queue(arr, "one-pack", output, 1, [2, 3]))
            arr.queue.return_value = [row, {**second, "statusMessages": [{"messages": ["other"]}]}]
            self.assertIsNone(empty_pack.no_files_queue(arr, "one-pack", output, 1, [2, 3]))
            arr.queue.return_value = [row, {**second, "episodeId": 6}]
            with self.assertRaisesRegex(LabFailure, "packQueueScopeMismatch"):
                empty_pack.no_files_queue(arr, "one-pack", output, 1, [2, 3])

    def test_reject_readback_requires_entire_original_job(self):
        arr = Mock()
        arr.queue.return_value = []
        quality = {"quality": {"id": 1}}
        row = {"id": 33, "eventType": "downloadFailed", "downloadId": "one-pack", "episodeId": 2,
               "seriesId": 1, "sourceTitle": "pack"}
        second = {**row, "id": 34, "episodeId": 3}
        block = {"id": 9, "seriesId": 1, "sourceTitle": "pack", "episodeIds": [3, 2],
                 "protocol": "usenet", "indexer": empty_pack.INDEXER_NAME, "quality": quality}
        arr.history.return_value = [row, second]
        arr.blocklist.return_value = [block]
        self.assertEqual(empty_pack.rejected_pack(arr, {"seriesId": 1}, "one-pack", "pack", [2, 3], quality, set()),
                         {"failureHistory": [row, second], "blocklist": block, "arrQueue": []})
        for defect in ("queue", "library", "blocklist"):
            with self.subTest(defect=defect):
                arr.queue.return_value = [{"id": 60}] if defect == "queue" else []
                arr.history.return_value = [row] if defect == "library" else [row, second]
                arr.blocklist.return_value = [{**block, "episodeIds": [2]}] if defect == "blocklist" else [block]
                if defect == "blocklist":
                    with self.assertRaisesRegex(LabFailure, "packBlocklistScopeMismatch"):
                        empty_pack.rejected_pack(arr, {"seriesId": 1}, "one-pack", "pack", [2, 3], quality, set())
                else:
                    self.assertIsNone(empty_pack.rejected_pack(arr, {"seriesId": 1}, "one-pack", "pack", [2, 3], quality, set()))

    def test_scenario_keeps_initial_auto_import_off_until_fixture_is_empty(self):
        with patch("scenarios.Arr") as arr, patch("scenarios.wait_for"), patch("scenarios.setup_fixture_dirs"), patch(
            "scenarios.configure_sab"
        ), patch("scenarios.get_json"), patch("empty_pack.empty_pack_replacement") as run:
            arr.return_value.setup.return_value = {"id": 1, "cutoff": 1001}
            arr.return_value.media.return_value = [{"id": 1}, {"id": 2}]
            evidence = {}
            self.assertTrue(run_scenarios(evidence, probe.SONARR_V5_EMPTY_PACK))
            arr.return_value.setup.assert_called_once_with(automatic_redownload=True, completed_handling=False)
            run.assert_called_once_with(arr.return_value, {"id": 1}, {"id": 2}, evidence["scenarios"][probe.SONARR_V5_EMPTY_PACK])


class GoPackProbeTest(unittest.TestCase):
    def test_scope_and_credential_use_stdin(self):
        arr = Mock(headers={"X-Api-Key": "test-only-credential"})
        scope = {"QueueIDs": [6, 8], "DownloadID": "sealed-job", "Release": "lab-pack",
                 "SeriesID": 2, "EpisodeIDs": [1, 2]}
        with patch("empty_pack.subprocess.run", return_value=subprocess.CompletedProcess(
            args=[], returncode=0, stdout='{"ok":true,"commandId":42}', stderr=""
        )) as run:
            self.assertEqual(empty_pack.run_go_adapter(arr, "search", scope, episodeIds=[1]), {"ok": True, "commandId": 42})
            self.assertEqual(run.call_args.args, (["/lab-tools/go-pack-probe"],))
            self.assertEqual(json.loads(run.call_args.kwargs["input"]), {
                "operation": "search", "apiKey": "test-only-credential", "scope": scope, "episodeIds": [1],
            })

    def test_exports_only_known_error_categories(self):
        arr = Mock(headers={"X-Api-Key": "test-only-credential"})
        for reply, expected in (
            ('{"ok":false,"reason":"reject:uncertainDispatch"}', "goPackProbe:reject:uncertainDispatch"),
            ('{"ok":false,"reason":"private output path"}', "goPackProbeFailed"),
            ('{"ok":false,"reason":[]}', "goPackProbeFailed"),
            ('[]', "goPackProbeFailed"),
        ):
            with self.subTest(reply=reply), patch("empty_pack.subprocess.run", return_value=subprocess.CompletedProcess(
                args=[], returncode=1, stdout=reply, stderr="private storage path"
            )), self.assertRaisesRegex(LabFailure, "^" + expected + "$"):
                empty_pack.run_go_adapter(arr, "reject", {})
