import io
import json
import tempfile
import unittest
import xml.etree.ElementTree as ET
from contextlib import redirect_stdout
from pathlib import Path
from unittest.mock import Mock, patch

import probe
from scenarios import CATEGORY, ID_ONLY_WARNING, LabFailure, id_only_import, run_scenarios
from source import ID_ONLY_PHASE, ID_ONLY_TARGET, TVDB_ID, article_id, metadata_path_kind, nzb, releases, rss, series_metadata, valid_phase


class SonarrV5LabTest(unittest.TestCase):
    def test_feed_uses_abbreviated_series_title_and_retains_exact_tvdb_id(self):
        title = releases(ID_ONLY_TARGET, ID_ONLY_PHASE)
        self.assertEqual(title, "Rescuarr.Synthetic.S01E01.720p.WEB-DL.x264-LABIDMATCH")
        items = ET.fromstring(rss([(ID_ONLY_TARGET, ID_ONLY_PHASE, 42)], "sonarr", 123)).findall("./channel/item")
        self.assertEqual(len(items), 1)
        self.assertEqual(items[0].findtext("title"), title)
        attributes = {node.attrib["name"]: node.attrib["value"] for node in items[0]
                      if node.tag.endswith("}attr")}
        self.assertEqual(attributes["tvdbid"], str(TVDB_ID))
        namespace = {"nzb": "http://www.newzbin.com/DTD/2003/nzb"}
        article = ET.fromstring(nzb(ID_ONLY_TARGET, ID_ONLY_PHASE, b"synthetic")).findtext(".//nzb:segment", namespaces=namespace)
        self.assertEqual(article, article_id(ID_ONLY_TARGET, ID_ONLY_PHASE, 1))
        self.assertFalse(valid_phase("episode2", ID_ONLY_PHASE))
        self.assertFalse(valid_phase("movie1", ID_ONLY_PHASE))

    def test_v5_metadata_contract_keeps_v4_and_checks_language_and_order(self):
        self.assertEqual(metadata_path_kind(f"/v1/tvdb/shows/en/{TVDB_ID}"), "sonarrSeries")
        self.assertEqual(metadata_path_kind(f"/v2/tvdb/shows/eng/{TVDB_ID}/official"), "sonarrSeries")
        self.assertEqual(metadata_path_kind(f"/v2/tvdb/shows/eng/{TVDB_ID}/dvd"), "unmatchedMetadata")
        self.assertEqual(metadata_path_kind(f"/v2/tvdb/shows/zho/{TVDB_ID}/official"), "unmatchedMetadata")
        self.assertEqual(series_metadata()["seasonTypes"][0]["episodeCount"], 2)

    def test_probe_selects_v5_only_for_the_new_scenario(self):
        for scenario, version in ((probe.SONARR_V5_ID_IMPORT, "5.0.0.2941"), ("sonarr/import", "4.0.20.3014")):
            with self.subTest(scenario=scenario), redirect_stdout(io.StringIO()) as output, patch(
                "sys.argv", ["probe.py", "fixture-sha", "fixture-image", scenario]
            ), patch("probe.arr_snapshot", return_value={"version": version}) as snapshot, patch(
                "probe.sab_snapshot", return_value={}
            ), patch("scenarios.run_scenarios", return_value=True) as run:
                self.assertEqual(probe.main(), 0)
                snapshot.assert_any_call("sonarr", 8989, version, "includeUnknownSeriesItems")
                self.assertEqual(run.call_args.args[1], scenario)
                self.assertEqual(json.loads(output.getvalue())["sourceSHA"], "fixture-sha")

    def test_v5_queue_keeps_completed_handling_enabled(self):
        for scenario, expected in ((probe.SONARR_V5_ID_IMPORT, True), ("sonarr/import", False)):
            with self.subTest(scenario=scenario), patch("scenarios.Arr") as arr, patch(
                "scenarios.wait_for"
            ), patch("scenarios.setup_fixture_dirs"), patch("scenarios.configure_sab"), patch(
                "scenarios.get_json"
            ), patch("scenarios.id_only_import") as id_only, patch("scenarios.original_import") as original:
                arr.return_value.setup.return_value = {"id": 1, "cutoff": 1001}
                arr.return_value.media.return_value = [{}, {}]
                evidence = {}
                self.assertTrue(run_scenarios(evidence, scenario))
                arr.return_value.setup.assert_called_once_with(automatic_redownload=True, completed_handling=expected)
                self.assertEqual(evidence["scenarios"][scenario]["settings"]["completedHandling"], expected)
                self.assertEqual(id_only.called, expected)
                self.assertEqual(original.called, not expected)

    def test_import_requires_real_block_then_exact_copy_and_independent_readback(self):
        for defect in (None, "wrong_reason", "wrong_file_id", "changed_source"):
            with self.subTest(defect=defect), tempfile.TemporaryDirectory() as temporary:
                downloads = Path(temporary).resolve()
                title = releases(ID_ONLY_TARGET, ID_ONLY_PHASE)
                folder = downloads / "complete" / title
                folder.mkdir(parents=True)
                source = folder / (title + ".mkv")
                source.write_bytes(b"fixture video bytes")
                guard_file = folder / "extra.txt"
                guard_file.write_text("retain this", encoding="utf-8")
                library = downloads / "library"
                library.mkdir()
                installed = library / "renamed.mkv"
                upstream_installed = "/library/renamed.mkv"
                media = {"id": 30, "seriesId": 20, "seasonNumber": 1}
                guard = {"id": 31, "seriesId": 20, "seasonNumber": 1}
                download_id = "fixture-job"
                grabbed = {"id": 40, "eventType": "grabbed", "seriesId": 20, "episodeId": 30,
                           "downloadId": download_id, "data": {"SeriesMatchType": "Id", "releaseSource": "UserInvokedSearch"}}
                candidate = {"path": str(source), "downloadId": download_id,
                             "quality": {"quality": {"id": 5}}, "languages": [{"id": 1}],
                             "releaseGroup": "LABIDMATCH", "releaseType": "singleEpisode", "indexerFlags": 0}
                row = {"id": 50, "downloadId": download_id, "outputPath": str(folder), "status": "completed",
                       "seriesId": 20, "episodeId": 30, "trackedDownloadState": "importBlocked", "statusMessages": [
                           {"messages": ["Another warning." if defect == "wrong_reason" else ID_ONLY_WARNING]}]}
                history = {"id": 41, "eventType": "downloadFolderImported", "seriesId": 20, "episodeId": 30,
                           "downloadId": download_id, "data": {"fileId": "99" if defect == "wrong_file_id" else "60",
                                                                "droppedPath": str(source), "importedPath": upstream_installed}}
                arr = Mock(product="sonarr", library=library)
                arr.grab.return_value = ({"status": "completed"}, grabbed)
                arr.queue.side_effect = lambda supplied_id: [] if installed.exists() else [row]
                arr.history.side_effect = lambda **query: (
                    [] if query.get("episodeId") == guard["id"] else [grabbed, history] if installed.exists() else [grabbed])
                arr.file_link.side_effect = lambda target: 60 if target == media["id"] and installed.exists() else 0
                arr.read.return_value = [{**candidate, "rejections": [{"type": "permanent"}],
                                          "series": {"id": 20}, "folderName": title}]
                arr.api.return_value = (202, [{**candidate, "seriesId": 20, "seasonNumber": 1,
                                               "episodes": [{"id": 30}], "rejections": []}])
                arr.library_file.return_value = {"id": 60, "path": upstream_installed}

                def copy(name, **kwargs):
                    self.assertEqual(name, "ManualImport")
                    self.assertEqual(kwargs["importMode"], "copy")
                    self.assertEqual(kwargs["files"], [{**candidate, "seriesId": 20, "episodeIds": [30], "folderName": title}])
                    installed.write_bytes(source.read_bytes())
                    if defect == "changed_source":
                        guard_file.unlink()
                    return {"status": "completed"}

                arr.command_status.side_effect = copy
                report = {}
                with patch("scenarios.DOWNLOADS", downloads), patch("scenarios.call_json"), patch(
                    "scenarios.search_events", return_value=[]
                ), patch("scenarios.completed_job", return_value={"name": title, "storage": str(folder), "category": CATEGORY["sonarr"]}), patch(
                    "scenarios.sab_job", return_value={"queue": [], "history": [{"nzo_id": download_id}], "archive": []}
                ):
                    if defect:
                        expected = {"wrong_reason": "blockedForAnotherReason", "wrong_file_id": "importHistoryScopeMismatch",
                                    "changed_source": "IDOnlyImportChangedSource"}[defect]
                        with self.assertRaisesRegex(LabFailure, expected):
                            id_only_import(arr, media, guard, report)
                    else:
                        id_only_import(arr, media, guard, report)
                        self.assertTrue(report["sourcePreserved"])
                        self.assertTrue(report["reprocessUnchanged"])
                        self.assertEqual(report["after"]["history"]["fileId"], 60)
                        self.assertEqual(report["after"]["guardFileId"], 0)
                if defect == "wrong_reason":
                    arr.api.assert_not_called()
                    arr.command_status.assert_not_called()
                else:
                    arr.api.assert_called_once_with("manualimport", method="POST", body=[
                        {**candidate, "seriesId": 20, "seasonNumber": 1, "episodeIds": [30]}])
                    arr.command_status.assert_called_once()
