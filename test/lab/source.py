"""Fixed metadata, Newznab and NNTP responses for the internal lab network."""

import binascii
import email.utils
import hashlib
import itertools
import json
import re
import socketserver
import ssl
import threading
import xml.etree.ElementTree as ET
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

SOURCE = "http://source:8081"
SEED = "rescuarr-lab-v1"
TVDB_ID = 91000001
TMDB_IDS = (92000001, 92000002)
SONARR_TITLE = "Rescuarr Synthetic Series"
RADARR_TITLES = ("Rescuarr Synthetic Film", "Rescuarr Synthetic Sequel")
NEWS_GROUP = "alt.binaries.rescuarr-lab"
NEWS_DOMAIN = "rescuarr.invalid"
ENCLOSURE_TYPE = "application/x-nzb"
NEWZNAB_NS = "http://www.newznab.com/DTD/2010/feeds/attributes/"
SEGMENT_BYTES = 262_144
RELEASE_GROUPS = {
    "original": "LABORIGINAL",
    "terminal-original": "LABORIGINAL",
    "replacement": "LABREPLACEMENT",
}
ARTICLE_ID = re.compile(
    rb"<" + re.escape(SEED.encode("ascii")) + rb"\.(episode[12]|movie[12])\."
    rb"(original|terminal-original|replacement)\.(\d{1,5})@" + re.escape(NEWS_DOMAIN.encode("ascii")) + rb">"
)

# Each target can select its release fixture through the lab control route.
TARGETS = {
    "episode1": ("sonarr", f"{SONARR_TITLE.replace(' ', '.')}.S01E01", 5040),
    "episode2": ("sonarr", f"{SONARR_TITLE.replace(' ', '.')}.S01E02", 5040),
    "movie1": ("radarr", f"{RADARR_TITLES[0].replace(' ', '.')}.2020", 2040),
    "movie2": ("radarr", f"{RADARR_TITLES[1].replace(' ', '.')}.2020", 2040),
}

CAPABILITIES = b"""<?xml version="1.0" encoding="utf-8"?>
<caps><server title="Rescuarr Lab"/><limits max="100" default="100"/>
<searching>
  <search available="yes" supportedParams="q"/>
  <tv-search available="yes" supportedParams="q,tvdbid,season,ep"/>
  <movie-search available="yes" supportedParams="q,tmdbid"/>
</searching>
<categories><category id="2000" name="Movies"><subcat id="2040" name="HD"/></category>
<category id="5000" name="TV"><subcat id="5040" name="HD"/></category></categories>
</caps>"""


def series_metadata():
    return {
        "tvdbId": TVDB_ID,
        "title": SONARR_TITLE,
        "slug": "rescuarr-synthetic-series",
        "firstAired": "2020-01-01",
        "status": "ended",
        "originalLanguage": "en",
        "runtime": 15,
        "seasons": [{"seasonNumber": 1, "images": []}],
        "episodes": [
            {"tvdbId": TVDB_ID + number, "seasonNumber": 1, "episodeNumber": number, "title": f"Episode {number}",
             "airDate": "2020-01-01", "airDateUtc": "2020-01-01T00:00:00Z", "runtime": 15}
            for number in (1, 2)
        ],
    }


def movie_metadata(index):
    return {
        "tmdbId": TMDB_IDS[index],
        "title": RADARR_TITLES[index],
        "originalTitle": RADARR_TITLES[index],
        "year": 2020,
        "inCinema": "2020-01-01T00:00:00Z",
        "physicalRelease": "2020-01-01T00:00:00Z",
        "runtime": 15,
        "originalLanguage": "en",
        "alternativeTitles": [],
        "translations": [],
        "certifications": [],
        "images": [],
        "credits": {"cast": [], "crew": []},
        "ratings": [{"count": 1, "value": 1}],
    }


def releases(target, phase):
    prefix = TARGETS[target][1]
    group = RELEASE_GROUPS[phase]
    return f"{prefix}.720p.WEB-DL.x264-{group}"


def search_targets(product, query):
    kind, tvdb, season, episode, tmdb, title = (
        query.get(key, [""])[0] for key in ("t", "tvdbid", "season", "ep", "tmdbid", "q")
    )
    title = re.sub(r"[^a-z0-9]", "", title.lower())
    if product == "sonarr" and kind == "tvsearch":
        if tvdb not in ("", str(TVDB_ID)) or (not tvdb and "rescuarrsyntheticseries" not in title):
            return []
        if season in ("1", "01") and episode in ("1", "2"):
            return [f"episode{episode}"]
    if product == "radarr" and kind in ("movie", "search"):
        for index, movie_id in enumerate(TMDB_IDS):
            if tmdb == str(movie_id) or (not tmdb and re.sub("[^a-z]", "", RADARR_TITLES[index].lower()) in title):
                return [f"movie{index + 1}"]
    return []


def recent_index_targets(product, query):
    """The pinned indexers validate settings with an unscoped recent-feed read."""
    expected_kind, expected_category, sample = {
        "sonarr": ("tvsearch", "5040", "episode1"),
        "radarr": ("movie", "2040", "movie1"),
    }[product]
    if (query.get("t") not in ([expected_kind], ["search"])
            or expected_category not in query.get("cat", [""])[0].split(",")
            or any(query.get(key, [""])[0] for key in
                   ("q", "tvdbid", "rid", "imdbid", "tmdbid", "season", "ep"))):
        return []
    return [sample]


def rss(items, product, size):
    root = ET.Element("rss", version="2.0", attrib={"xmlns:newznab": NEWZNAB_NS})
    channel = ET.SubElement(root, "channel")
    ET.SubElement(channel, "title").text = "Rescuarr synthetic indexer"
    ET.SubElement(channel, "link").text = SOURCE
    ET.SubElement(channel, "description").text = "Disposable test releases"
    ET.SubElement(channel, "newznab:response", offset="0", total=str(len(items)))
    for target, phase, timestamp in items:
        url = f"{SOURCE}/nzb/{target}/{phase}"
        item = ET.SubElement(channel, "item")
        ET.SubElement(item, "title").text = releases(target, phase)
        ET.SubElement(item, "guid", isPermaLink="false").text = url
        ET.SubElement(item, "link").text = url
        ET.SubElement(item, "comments").text = url
        ET.SubElement(item, "pubDate").text = email.utils.formatdate(timestamp, usegmt=True)
        ET.SubElement(item, "enclosure", url=url, length=str(size), type=ENCLOSURE_TYPE)
        attributes = {"category": str(TARGETS[target][2]), "size": str(size)}
        if product == "sonarr":
            attributes["tvdbid"] = str(TVDB_ID)
        else:
            attributes["tmdbid"] = str(TMDB_IDS[int(target[-1]) - 1])
        for key, value in attributes.items():
            ET.SubElement(item, "newznab:attr", name=key, value=value)
    return ET.tostring(root, encoding="utf-8", xml_declaration=True)


def article_id(target, phase, number):
    return f"{SEED}.{target}.{phase}.{number}@{NEWS_DOMAIN}"


def nzb(target, phase, media):
    name = releases(target, phase) + ".mkv"
    root = ET.Element("nzb", xmlns="http://www.newzbin.com/DTD/2003/nzb")
    date = str(int(datetime.now(timezone.utc).timestamp()))
    file = ET.SubElement(root, "file", poster=f"Rescuarr <lab@{NEWS_DOMAIN}>", date=date,
                         subject=f'"{name}" yEnc')
    ET.SubElement(ET.SubElement(file, "groups"), "group").text = NEWS_GROUP
    segments = ET.SubElement(file, "segments")
    for number, offset in enumerate(range(0, len(media), SEGMENT_BYTES), 1):
        ET.SubElement(segments, "segment", bytes=str(len(media[offset:offset + SEGMENT_BYTES])),
                      number=str(number)).text = article_id(target, phase, number)
    return ET.tostring(root, encoding="utf-8", xml_declaration=True)


def encoded_lines(data):
    encoded = bytearray()
    for byte in data:
        shifted = (byte + 42) % 256
        if shifted in (0, 10, 13, 61):
            encoded.extend((61, (shifted + 64) % 256))
        else:
            encoded.append(shifted)
        if len(encoded) >= 128:
            yield bytes(encoded)
            encoded.clear()
    if encoded:
        yield bytes(encoded)


def news_article(target, phase, number, media):
    offset = (number - 1) * SEGMENT_BYTES
    piece = media[offset:offset + SEGMENT_BYTES]
    if number < 1 or not piece:
        return None
    name = releases(target, phase) + ".mkv"
    parts = (len(media) + SEGMENT_BYTES - 1) // SEGMENT_BYTES
    crc = f"{binascii.crc32(piece):08x}"
    total_crc = f" crc32={binascii.crc32(media):08x}" if number == parts else ""
    return itertools.chain(
        (f"=ybegin part={number} total={parts} line=128 size={len(media)} name={name}".encode("ascii"),
         f"=ypart begin={offset + 1} end={offset + len(piece)}".encode("ascii")),
        encoded_lines(piece),
        (f"=yend size={len(piece)} part={number} pcrc32={crc}{total_crc}".encode("ascii"),),
    )


class News(socketserver.StreamRequestHandler):
    def handle(self):
        self.connection.settimeout(30)
        self.line(b"201 Rescuarr LAB NNTP reader")
        for line in iter(lambda: self.rfile.readline(1024), b""):
            command, _, arg = line.rstrip().partition(b" ")
            command = command.upper()
            if command == b"QUIT":
                self.line(b"205 Closing")
                return
            if command == b"CAPABILITIES":
                self.line(b"101 Capabilities")
                self.multiline((b"VERSION 2", b"READER"))
            elif command == b"MODE":
                self.line(b"201 Reader")
            elif command == b"GROUP" and arg == NEWS_GROUP.encode("ascii"):
                self.line(b"211 0 0 0 " + arg)
            elif command in (b"BODY", b"ARTICLE", b"STAT"):
                self.reply_article(command, arg)
            else:
                self.line(b"500 Lab command unknown")

    def line(self, line):
        self.wfile.write(line + b"\r\n")

    def multiline(self, lines):
        for line in lines:
            self.line(b"." + line if line.startswith(b".") else line)
        self.line(b".")

    def reply_article(self, command, message_id):
        match = ARTICLE_ID.fullmatch(message_id)
        if match is None:
            self.line(b"430 No such synthetic article")
            return
        target, phase, number = (value.decode("ascii") for value in match.groups())
        number = int(number)
        article = news_article(target, phase, number, self.server.media)
        if article is None:
            self.line(b"430 No such synthetic article")
        elif phase == "terminal-original":
            with self.server.lock:
                self.server.missing_articles[target].add((number, command.decode("ascii")))
            self.line(b"430 No such synthetic article")
        elif command == b"STAT":
            self.line(b"223 0 " + message_id)
        else:
            code = b"222" if command == b"BODY" else b"220"
            self.line(code + b" 0 " + message_id)
            if command == b"ARTICLE":
                self.line(b"Message-ID: " + message_id)
                self.line(b"Newsgroups: " + NEWS_GROUP.encode("ascii"))
                self.line(b"")
            self.multiline(article)


class Source(BaseHTTPRequestHandler):
    def log_message(self, *args):
        # HTTP inputs can have credential query strings. The scenario captures selected keys.
        return

    def write(self, data, content_type="application/json"):
        if not isinstance(data, bytes):
            data = json.dumps(data).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        path = urlsplit(self.path).path
        match = re.fullmatch(r"/lab/feed/(episode[12]|movie[12])/(original|terminal-original|replacement)", path)
        if not match:
            self.send_error(404)
            return
        target, phase = match.groups()
        with self.server.lock:
            self.server.phases[target] = phase
        self.write({"target": target, "phase": phase})

    def do_GET(self):
        url = urlsplit(self.path)
        path, query = url.path, parse_qs(url.query)
        if path == "/health":
            self.write({"seed": SEED})
        elif path == f"/v1/tvdb/shows/en/{TVDB_ID}":
            self.write(series_metadata())
        elif path in (f"/v1/movie/{movie_id}" for movie_id in TMDB_IDS):
            self.write(movie_metadata(TMDB_IDS.index(int(path.split("/")[-1]))))
        elif path in ("/sonarr/api", "/radarr/api"):
            self.write_index(path.split("/")[1], query)
        elif path == "/lab/searches":
            with self.server.lock:
                self.write(list(self.server.searches))
        elif (match := re.fullmatch(r"/lab/missing-articles/(episode[12]|movie[12])", path)):
            target = match[1]
            with self.server.lock:
                replies = sorted(self.server.missing_articles[target])
            self.write([
                {"messageId": article_id(target, "terminal-original", number), "command": command, "responseCode": 430}
                for number, command in replies
            ])
        elif path == "/lab/media":
            self.write(self.server.media_fingerprint)
        elif (match := re.fullmatch(r"/nzb/(episode[12]|movie[12])/(original|terminal-original|replacement)", path)):
            self.write(nzb(*match.groups(), self.server.media), ENCLOSURE_TYPE)
        else:
            self.send_error(404)

    def write_index(self, product, query):
        kind = query.get("t", [""])[0]
        if kind == "caps":
            self.write(CAPABILITIES, "application/xml")
            return
        scope = search_targets(product, query)
        items = []
        if query.get("offset", ["0"])[0] == "0":
            with self.server.lock:
                self.server.searches.append({"product": product, "kind": kind,
                                             "targets": scope, "season": query.get("season", [""])[0],
                                             "episode": query.get("ep", [""])[0]})
                for target in scope:
                    phase = self.server.phases.get(target, "original")
                    stamp = self.server.started - (60 if phase == "replacement" else 3600)
                    items.append((target, phase, stamp))
            # Validation needs a parseable recent item. It remains outside the
            # recorded target scope and cannot satisfy a recovery search.
            if not items:
                items.extend((target, "original", self.server.started - 3600)
                             for target in recent_index_targets(product, query))
        self.write(rss(items, product, len(self.server.media)), "application/rss+xml")


class NNTPServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


def main():
    media = Path("/media/synthetic.mkv").read_bytes()
    plain = ThreadingHTTPServer(("0.0.0.0", 8081), Source)
    secure = ThreadingHTTPServer(("0.0.0.0", 443), Source)
    news = NNTPServer(("0.0.0.0", 8119), News)
    # The fixture state is shared between both HTTP listeners.
    lock = threading.Lock()
    phases, searches = {}, []
    missing_articles = {target: set() for target in TARGETS}
    media_fingerprint = {"bytes": len(media), "sha256": hashlib.sha256(media).hexdigest()}
    started = datetime.now(timezone.utc).timestamp()
    for server in (plain, secure):
        server.lock = lock
        server.phases = phases
        server.searches = searches
        server.media = media
        server.media_fingerprint = media_fingerprint
        server.missing_articles = missing_articles
        server.started = started
    news.lock = lock
    news.missing_articles = missing_articles
    news.media = media
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.load_cert_chain("/tls/source.pem", "/tls/source.key")
    secure.socket = tls.wrap_socket(secure.socket, server_side=True)
    for server in (news, secure):
        threading.Thread(target=server.serve_forever, daemon=True).start()
    plain.serve_forever()


if __name__ == "__main__":
    main()
