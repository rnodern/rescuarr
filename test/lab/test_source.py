import binascii
import io
import unittest
import xml.etree.ElementTree as ET

from source import (
    CAPABILITIES,
    NEWS_DOMAIN,
    NEWS_GROUP,
    SEGMENT_BYTES,
    SEED,
    SOURCE,
    TMDB_IDS,
    TVDB_ID,
    article_id,
    movie_metadata,
    metadata_path_kind,
    news_article,
    nzb,
    rss,
    recent_index_targets,
    search_targets,
    series_metadata,
)

from scenarios import validation_category


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

    def test_recent_validation_feed_is_distinct_from_targeted_search(self):
        tv = {"t": ["tvsearch"], "cat": ["5040"], "offset": ["0"]}
        movie = {"t": ["movie"], "cat": ["2040"]}
        self.assertEqual(recent_index_targets("sonarr", tv), ["episode1"])
        self.assertEqual(recent_index_targets("radarr", movie), ["movie1"])
        self.assertEqual(search_targets("sonarr", tv), [])
        self.assertEqual(search_targets("radarr", movie), [])
        self.assertEqual(recent_index_targets("sonarr", {**tv, "ep": ["3"]}), [])
        self.assertEqual(recent_index_targets("radarr", {**movie, "tmdbid": ["1"]}), [])
        self.assertEqual(recent_index_targets("radarr", {**movie, "cat": ["5000"]}), [])

    def test_indexer_validation_diagnostics_exclude_upstream_text(self):
        example = io.BytesIO(b'[{"errorMessage":"Unable to connect to http://private.invalid/?apikey=secret"}]')
        self.assertEqual(validation_category(example), "cannotConnect")
        self.assertEqual(validation_category(io.BytesIO(b'{}')), "validationUnknown")

    def test_metadata_diagnostics_use_fixed_buckets(self):
        self.assertEqual(metadata_path_kind(f"/v1/tvdb/shows/en/{TVDB_ID}"), "sonarrSeries")
        self.assertEqual(metadata_path_kind(f"/v1/movie/{TMDB_IDS[0]}"), "radarrMovie")
        self.assertEqual(metadata_path_kind("/v1/other/private-id"), "unmatchedMetadata")
        self.assertIsNone(metadata_path_kind("/sonarr/api"))

    def test_index_item_binds_a_scoped_nzb(self):
        caps = ET.fromstring(CAPABILITIES)
        self.assertEqual(caps.find("./searching/movie-search").attrib["available"], "yes")
        feed = ET.fromstring(rss([("episode1", "replacement", 42)], "sonarr", 123))
        items = feed.findall("./channel/item")
        self.assertEqual(len(items), 1)
        self.assertIn("S01E01", items[0].findtext("title"))
        self.assertEqual(items[0].find("enclosure").attrib["url"], f"{SOURCE}/nzb/episode1/replacement")

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
