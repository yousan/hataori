"""Exercise both offline scrubbers with actual captured transport records."""
import copy
import json
from pathlib import Path
import runpy
import unittest

from capture_metadata import normalize_metadata
from supervisor_gate import make_scrubber

HERE = Path(__file__).resolve().parent
ACTUAL = HERE.parents[3] / "docs/notes/codex-supervisor-1088/scrub_actual.py"
actual_scrubber = runpy.run_path(str(ACTUAL))["make_scrubber"]


class CaptureMetadataTest(unittest.TestCase):
    def test_both_writers_normalize_nested_captured_metadata(self):
        for name in ("supervisor_gate", "supervisor_direct"):
            rows = [json.loads(line) for line in
                    (HERE / "0.163.0" / name / "wire.jsonl").read_text().splitlines()]
            remote = [row for row in rows if row["msg"].get("method") ==
                      "remoteControl/status/changed"]
            self.assertEqual(len(remote), 2)
            for row in remote:
                for factory in (make_scrubber, lambda: actual_scrubber("/fixture/export")):
                    with self.subTest(capture=name, writer=factory):
                        # Synthetic identity, real envelope and metadata fields.
                        source = copy.deepcopy(row)
                        source["msg"]["params"]["serverName"] = "capture-host.invalid"
                        wrapped = {"records": [source]}
                        expected = copy.deepcopy(wrapped)
                        expected["records"][0]["msg"]["params"]["serverName"] = "fixture-server"
                        result = factory()(wrapped)
                        self.assertTrue(result == factory()(expected),
                                        "writer retained nested deployment metadata")
                        self.assertTrue(source["msg"]["params"]["serverName"] ==
                                        "capture-host.invalid", "writer mutated its input")
                        # Only the identifying name changes; IDs, status, envelope
                        # and evidence fields remain structurally identical.
                        self.assertTrue(normalize_metadata(wrapped) == expected,
                                        "normalization changed behavioral evidence")
                        for empty in (None, ""):
                            source["msg"]["params"]["serverName"] = empty
                            self.assertTrue(normalize_metadata(source) == source,
                                            "normalization changed absent metadata")

    def test_all_sibling_captures_are_normalized(self):
        count = 0
        for directory in sorted((HERE / "0.163.0").glob("supervisor_*")):
            for path in sorted(directory.rglob("*")):
                if path.suffix not in (".json", ".jsonl"):
                    continue
                text = path.read_text()
                rows = ([json.loads(line) for line in text.splitlines()]
                        if path.suffix == ".jsonl" else [json.loads(text)])
                for row in rows:
                    # Never print an actual value, including on failure.
                    self.assertTrue(normalize_metadata(row) == row,
                                    f"unnormalized remote metadata in {path.name}")
                    count += 1
        self.assertGreater(count, 0)


if __name__ == "__main__":
    unittest.main()
