import unittest
from pathlib import PureWindowsPath
from unittest.mock import patch

from language_pack import pack_storage
from scenarios import LabFailure
from source import EMPTY_PACK_TARGET, LANGUAGE_PACK_PHASE, article_filename, releases


class LanguagePackFixtureTest(unittest.TestCase):
    def test_pack_storage_is_the_named_download_folder(self):
        title = releases(EMPTY_PACK_TARGET, LANGUAGE_PACK_PHASE)
        folder = PureWindowsPath(r"C:\lab\downloads\complete") / title
        video = folder / article_filename("episode1", LANGUAGE_PACK_PHASE)
        with patch("language_pack.safe_download_path", side_effect=PureWindowsPath):
            self.assertEqual(pack_storage(str(folder), title), folder)
            self.assertEqual(pack_storage(str(video), title), folder)
            with self.assertRaisesRegex(LabFailure, "languagePackSABPathMismatch"):
                pack_storage(str(folder / "another-project.mkv"), title)
            with self.assertRaisesRegex(LabFailure, "languagePackSABPathMismatch"):
                pack_storage(str(folder.parent / "another-project" / video.name), title)
