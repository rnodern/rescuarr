import binascii
import io
import threading
import unittest
import xml.etree.ElementTree as ET
from types import SimpleNamespace

from source import (
    CAPABILITIES,
    NEWS_DOMAIN,
    NEWS_GROUP,
    News,
    SEGMENT_BYTES,
    SEED,
    SOURCE,
    TMDB_IDS,
    TVDB_ID,
    article_id,
    movie_metadata,
    news_article,
    nzb,
    releases,
    rss,
    search_targets,
    series_metadata,
)


class SyntheticSourceTest(unittest.TestCase):
    def test_tv_search_does_not_leak_other_episodes(self):
        query = {"t": ["tvsearch"], "tvdbid": [str(TVDB_ID)], "season": ["01"], "ep": ["2"]}
        self.assertEqual(search_targets("sonarr", query), ["episode2"])
        self.assertEqual(search_targets("sonarr", {**query, "ep": ["3"]}), [])
        self.assertEqual(search_targets("sonarr", {**query, "tvdbid": ["1"]}), [])
        self.assertEqual(search_targets("sonarr", {"t": ["search"]}), [])

    def test_movie_search_does_not_leak_other_titles(self):
        for index, movie_id in enumerate(TMDB_IDS, 1):
            self.assertEqual(search_targets("radarr", {"t": ["movie"], "tmdbid": [str(movie_id)]}),
                             [f"movie{index}"])
            self.assertEqual(movie_metadata(index - 1)["tmdbId"], movie_id)
        self.assertEqual(search_targets("radarr", {"t": ["movie"], "tmdbid": ["1"]}), [])
        self.assertEqual(series_metadata()["tvdbId"], TVDB_ID)

    def test_index_item_binds_a_scoped_nzb(self):
        caps = ET.fromstring(CAPABILITIES)
        self.assertEqual(caps.find("./searching/movie-search").attrib["available"], "yes")
        feed = ET.fromstring(rss([("episode1", "replacement", 42)], "sonarr", 123))
        items = feed.findall("./channel/item")
        self.assertEqual(len(items), 1)
        self.assertIn("S01E01", items[0].findtext("title"))
        self.assertEqual(items[0].find("enclosure").attrib["url"], f"{SOURCE}/nzb/episode1/replacement")

    def test_terminal_release_returns_missing_articles_without_affecting_replacement(self):
        media = b"synthetic media"
        namespace = {"nzb": "http://www.newzbin.com/DTD/2003/nzb"}
        targets = {"episode1": "sonarr", "movie1": "radarr"}
        for target, product in targets.items():
            with self.subTest(product=product):
                feed = ET.fromstring(rss([(target, "terminal-original", 42)], product, len(media)))
                item = feed.find("./channel/item")
                self.assertEqual(item.findtext("title"), releases(target, "terminal-original"))
                self.assertEqual(item.find("enclosure").get("url"), f"{SOURCE}/nzb/{target}/terminal-original")
                handler = News.__new__(News)
                handler.server = SimpleNamespace(media=media, lock=threading.Lock(), missing_articles={target: set()})
                for phase, code in (("terminal-original", b"430"), ("original", b"222"), ("replacement", b"222")):
                    entry = ET.fromstring(nzb(target, phase, media))
                    article = entry.findtext(".//nzb:segment", namespaces=namespace)
                    self.assertEqual(article, article_id(target, phase, 1))
                    handler.wfile = io.BytesIO()
                    handler.reply_article(b"BODY", f"<{article}>".encode("ascii"))
                    response = handler.wfile.getvalue()
                    self.assertTrue(response.startswith(code + b" "))
                    if phase == "terminal-original":
                        self.assertEqual(response.splitlines(), [b"430 No such synthetic article"])
                    else:
                        self.assertIn(b"=ybegin", response)
                        self.assertTrue(response.endswith(b".\r\n"))
                self.assertEqual(handler.server.missing_articles[target], {(1, "BODY")})

    def test_nzb_segments_match_exact_yenc_articles(self):
        data = bytes(range(256)) * (SEGMENT_BYTES // 256 + 1)
        namespace = {"nzb": "http://www.newzbin.com/DTD/2003/nzb"}
        root = ET.fromstring(nzb("movie1", "original", data))
        group = root.findtext("nzb:file/nzb:groups/nzb:group", namespaces=namespace)
        self.assertEqual(group, NEWS_GROUP)
        segments = root.findall("nzb:file/nzb:segments/nzb:segment", namespaces=namespace)
        self.assertEqual([segment.text for segment in segments], [
            f"{SEED}.movie1.original.1@{NEWS_DOMAIN}", article_id("movie1", "original", 2),
        ])
        rebuilt = bytearray()
        for number, segment in enumerate(segments, 1):
            begin, limits, *payload, end = news_article("movie1", "original", number, data)
            self.assertIn(f"part={number}".encode(), begin)
            self.assertIn(f"begin={(number - 1) * SEGMENT_BYTES + 1}".encode(), limits)
            decoded = bytearray()
            encoded = iter(b"".join(payload))
            for byte in encoded:
                if byte == 61:
                    byte = (next(encoded) - 64) % 256
                decoded.append((byte - 42) % 256)
            self.assertEqual(len(decoded), int(segment.attrib["bytes"]))
            self.assertIn(f"pcrc32={binascii.crc32(decoded):08x}".encode(), end)
            rebuilt.extend(decoded)
        self.assertEqual(rebuilt, data)
        self.assertIsNone(news_article("movie1", "original", 3, data))


if __name__ == "__main__":
    unittest.main()
