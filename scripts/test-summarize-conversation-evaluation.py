"""只读第二批证据，所有破坏性案例均在内存或临时副本中执行。"""

import contextlib
import copy
import importlib.util
import io
import json
import shutil
import sys
import tempfile
import unittest
from pathlib import Path

sys.dont_write_bytecode = True
SCRIPT = Path(__file__).with_name("summarize-conversation-evaluation.py")
spec = importlib.util.spec_from_file_location("conversation_summary", SCRIPT)
summary = importlib.util.module_from_spec(spec)
spec.loader.exec_module(summary)
BASELINE = SCRIPT.parent.parent / "docs/evaluations/conversation-evidence/20260913T145156Z-61395637"


class ConversationSummaryTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="tripo-summary-offline-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name) / "evidence"
        shutil.copytree(BASELINE, self.directory)

    def read(self, name):
        return json.loads((self.directory / name).read_text())

    def write(self, name, value):
        (self.directory / name).write_text(json.dumps(value, ensure_ascii=False), encoding="utf-8")

    def test_second_batch_matches_entire_existing_summary(self):
        self.assertEqual(summary.summarize(self.directory), self.read("summary.json"))

    def technical_only_fixture(self):
        # 只在临时副本中模拟新批次；汇总器不能依据新标准自行重判旧审核。
        for name in ("manifest.json", "semantic-review.json"):
            document = self.read(name)
            document["evaluation_rubric"] = summary.TECHNICAL_ONLY_RUBRIC
            self.write(name, document)

    def test_technical_only_requires_explicit_matching_rubric_and_preserves_all_scores(self):
        previous = self.read("summary.json")
        self.technical_only_fixture()
        actual = summary.summarize(self.directory)
        self.assertEqual(actual["evaluation_rubric"], "technical-only-v1")
        self.assertEqual(actual["scope"], summary.TECHNICAL_ONLY_SCOPE)
        self.assertEqual(actual["counts"], previous["counts"])
        self.assertEqual(actual["rows"], previous["rows"])
        self.assertEqual(actual["status"], previous["status"])

    def test_rubric_is_not_inferred_when_only_one_side_declares_it(self):
        for declared, absent in (("manifest.json", "semantic-review.json"),
                                 ("semantic-review.json", "manifest.json")):
            with self.subTest(declared=declared):
                self.technical_only_fixture()
                document = self.read(absent)
                document.pop("evaluation_rubric")
                self.write(absent, document)
                with self.assertRaisesRegex(summary.EvidenceError, "evaluation_rubric"):
                    summary.summarize(self.directory)

    def test_unknown_null_or_mismatched_rubrics_are_rejected(self):
        for manifest_rubric, review_rubric in (
            ("future-v2", "future-v2"), (None, None), ("", ""),
            ("technical-only-v1", "future-v2"), ("technical-only-v1", None),
        ):
            with self.subTest(manifest=manifest_rubric, review=review_rubric):
                for name, rubric in (("manifest.json", manifest_rubric),
                                     ("semantic-review.json", review_rubric)):
                    document = self.read(name)
                    document["evaluation_rubric"] = rubric
                    self.write(name, document)
                with self.assertRaisesRegex(summary.EvidenceError, "evaluation_rubric"):
                    summary.summarize(self.directory)

    def test_technical_only_keeps_original_thresholds(self):
        self.technical_only_fixture()
        original = self.read("manifest.json")
        for dimension in summary.DIMENSIONS:
            with self.subTest(dimension=dimension):
                manifest = copy.deepcopy(original)
                manifest["thresholds"][dimension + "_pass_rate"] = 0.85
                self.write("manifest.json", manifest)
                with self.assertRaisesRegex(summary.EvidenceError, "门槛必须为 90%"):
                    summary.summarize(self.directory)
        original["thresholds"]["critical_errors"] = 1
        self.write("manifest.json", original)
        with self.assertRaisesRegex(summary.EvidenceError, "关键错误为零"):
            summary.summarize(self.directory)

    def test_technical_only_preserves_nonvisual_semantic_errors(self):
        self.technical_only_fixture()
        reviewed = self.read("semantic-review.json")
        name = "baseline-current-v3-product-1.json"
        reviewed["reviews"][name]["explanation"] = "failed"
        reviewed["reviews"][name]["reason"] = "供应商只返回原因未知，模型却断言失败由提示词导致。"
        reviewed["reviews"][name]["critical_errors"] = ["未经诊断的确定性因果归因"]
        self.write("semantic-review.json", reviewed)
        row = next(row for row in summary.summarize(self.directory)["rows"] if row["file"] == name)
        self.assertFalse(row["dimensions"]["explanation"])
        self.assertEqual(row["critical_errors"], ["semantic:未经诊断的确定性因果归因"])

    def test_cli_rejects_comparison_of_new_and_old_rubric_even_with_identical_scores(self):
        self.technical_only_fixture()
        output = Path(self.temporary.name) / "new-summary.json"
        error = io.StringIO()
        with contextlib.redirect_stderr(error):
            status = summary.main([str(self.directory), "--check-against",
                                   str(self.directory / "summary.json"), "--output", str(output)])
        self.assertEqual(status, 2)
        self.assertIn("计算结果与指定汇总不一致", error.getvalue())
        self.assertFalse(output.exists())

    def test_original_records_require_exact_manifest_coverage(self):
        name = "baseline-current-v3-product-1.json"
        value = self.read(name)
        (self.directory / name).unlink()
        with self.assertRaisesRegex(summary.EvidenceError, "原始记录覆盖不完整"):
            summary.summarize(self.directory)
        self.write(name, value)
        self.write("unexpected-case-1.json", value)
        with self.assertRaisesRegex(summary.EvidenceError, "多余=.*unexpected-case-1.json"):
            summary.summarize(self.directory)

    def test_nested_review_workpapers_do_not_count_as_extra_cases_or_duplicate_scores(self):
        reviews = self.read("semantic-review.json")["reviews"]
        for label in ("normal", "recovery"):
            self.write(f"semantic-baseline-{label}-review.json", {
                "reviewer": "offline-fixture", "reviews": reviews,
            })
        self.assertEqual(summary.summarize(self.directory), self.read("summary.json"))
        (self.directory / "semantic-review.json").unlink()
        with self.assertRaisesRegex(summary.EvidenceError, "semantic-review.json"):
            summary.summarize(self.directory)

    def test_raw_case_cannot_hide_behind_review_workpaper_name(self):
        name = "semantic-baseline-normal-review.json"
        self.write(name, self.read("baseline-current-v3-product-1.json"))
        with self.assertRaisesRegex(summary.EvidenceError, "多余=.*semantic-baseline-normal-review.json"):
            summary.summarize(self.directory)
        # 不仅完整记录；出现原始记录的任何顶层关键字段，都不能作为底稿跳过。
        for field in summary.RAW_RECORD_FIELDS:
            with self.subTest(field=field):
                self.write(name, {"reviewer": "offline-fixture", "reviews": {}, field: None})
                with self.assertRaisesRegex(summary.EvidenceError, "原始记录覆盖不完整"):
                    summary.summarize(self.directory)

    def test_run_observation_is_metadata_but_cannot_disguise_an_extra_case(self):
        name = "run-observation.json"
        self.write(name, {"status": "completed", "record_count": 84, "source_verified": True})
        self.assertEqual(summary.summarize(self.directory), self.read("summary.json"))
        for field in summary.RAW_RECORD_FIELDS:
            with self.subTest(field=field):
                self.write(name, {"status": "completed", field: None})
                with self.assertRaisesRegex(summary.EvidenceError, "多余=.*run-observation.json"):
                    summary.summarize(self.directory)

    def test_review_requires_every_case_and_repetition_without_extras(self):
        reviewed = self.read("semantic-review.json")
        name = "conversation-v3-explain_version-3.json"
        review = reviewed["reviews"].pop(name)
        self.write("semantic-review.json", reviewed)
        with self.assertRaisesRegex(summary.EvidenceError, "语义审核覆盖不完整"):
            summary.summarize(self.directory)
        reviewed["reviews"][name] = review
        reviewed["reviews"]["conversation-v3-explain_version-4.json"] = review
        self.write("semantic-review.json", reviewed)
        with self.assertRaisesRegex(summary.EvidenceError, "多余=.*explain_version-4.json"):
            summary.summarize(self.directory)

    def test_pending_review_and_missing_review_file_are_rejected(self):
        reviewed = self.read("semantic-review.json")
        reviewed["reviews"]["baseline-current-v3-product-1.json"]["strategy"] = "pending"
        self.write("semantic-review.json", reviewed)
        with self.assertRaisesRegex(summary.EvidenceError, "缺少已完成的语义审核"):
            summary.summarize(self.directory)
        (self.directory / "semantic-review.json").unlink()
        with self.assertRaisesRegex(summary.EvidenceError, "semantic-review.json"):
            summary.summarize(self.directory)

    def test_duplicate_json_review_keys_cannot_hide_coverage(self):
        path = self.directory / "semantic-review.json"
        original = path.read_text()
        path.write_text(original.replace('"reviews": {', '"reviews": {}, "reviews": {', 1))
        with self.assertRaisesRegex(summary.EvidenceError, "JSON 对象包含重复键"):
            summary.summarize(self.directory)

    def test_duplicate_manifest_and_mismatched_repeat_are_rejected(self):
        manifest = self.read("manifest.json")
        manifest["cases"].append(manifest["cases"][0])
        with self.assertRaisesRegex(summary.EvidenceError, "manifest 案例重复"):
            summary.expected_records(manifest)
        name = "baseline-current-v3-product-1.json"
        record = self.read(name)
        record["repeat"] = 2
        self.write(name, record)
        with self.assertRaisesRegex(summary.EvidenceError, "重复编号与 manifest 不一致"):
            summary.summarize(self.directory)

    def test_manifest_cannot_shrink_the_declared_eighty_four_targets(self):
        manifest = self.read("manifest.json")
        manifest["repetitions"] = 2
        self.write("manifest.json", manifest)
        with self.assertRaisesRegex(summary.EvidenceError, "共 84 个目标"):
            summary.summarize(self.directory)

    def test_each_constraint_failure_maps_to_its_actual_dimension_and_remains_critical(self):
        name = "baseline-current-v3-product-1.json"
        original = self.read(name)
        review = self.read("semantic-review.json")["reviews"][name]
        for check_name, dimension in summary.CONSTRAINT_DIMENSIONS.items():
            with self.subTest(check_name=check_name):
                record = copy.deepcopy(original)
                record["automated_checks"].append({"Name": check_name, "Dimension": "constraint",
                    "Passed": False, "Critical": True, "Detail": "seq=999 injected offline failure"})
                record["automated_passed"] = False
                record["critical_errors_automated"] = 1
                row = summary.summarize_record(name, record, record["case"], 1, review)
                self.assertEqual(row["dimensions"], {key: key != dimension for key in summary.DIMENSIONS})
                self.assertEqual(row["critical_errors"], [f"automated:{check_name}:seq=999 injected offline failure"])
                self.assertEqual(row["status"], "completed")  # Runtime 成功不能消除提议错误。
        with self.assertRaisesRegex(summary.EvidenceError, "尚未明确映射维度"):
            summary.check_dimension({"Dimension": "constraint", "Name": "future_unknown_rule"})

    def test_semantic_failure_survives_automated_runtime_success(self):
        rows = {row["file"]: row for row in summary.summarize(self.directory)["rows"]}
        recovered = rows["baseline-current-v3-faces_1000-2.json"]
        self.assertTrue(recovered["automated_passed"])
        self.assertEqual(recovered["status"], "completed")
        self.assertFalse(recovered["dimensions"]["strategy"])
        self.assertEqual(len(rows["baseline-current-v3-interior-2.json"]["critical_errors"]), 1)

    def test_not_applicable_requires_explicit_marker_and_unknown_runtime_fact(self):
        name = "baseline-current-v3-unknown-1.json"
        record = self.read(name)
        review = self.read("semantic-review.json")["reviews"][name]
        row = summary.summarize_record(name, record, record["case"], 1, review)
        self.assertIsNone(row["dimensions"]["explanation"])
        explicit_failure = {**review, "explanation": "failed"}
        self.assertFalse(summary.summarize_record(name, record, record["case"], 1, explicit_failure)
                         ["dimensions"]["explanation"])
        record["snapshot"]["session"]["result"]["reason"] = "model_error"
        with self.assertRaisesRegex(summary.EvidenceError, "not_applicable 仅限"):
            summary.summarize_record(name, record, record["case"], 1, review)

    def test_response_tokens_are_recorded_usage_and_requests_are_independent(self):
        name = "baseline-current-v3-product-1.json"
        record = self.read(name)
        review = self.read("semantic-review.json")["reviews"][name]
        original_calls = record["snapshot"]["session"]["model_calls"]
        record["snapshot"]["session"]["model_calls"] += 1
        events = record["snapshot"]["events"]
        events.append({"seq": events[-1]["seq"] + 1, "kind": "model_call", "data": {}})
        row = summary.summarize_record(name, record, record["case"], 1, review)
        self.assertEqual(row["model_calls"], original_calls + 1)
        self.assertEqual(row["recorded_response_count"], original_calls)
        self.assertEqual(row["recorded_completion_tokens"],
                         sum(item["response_meta"]["usage"]["completion_tokens"] for item in record["response_metadata"]))
        record["response_metadata"][0]["response_meta"].pop("usage")
        with self.assertRaisesRegex(summary.EvidenceError, "响应缺少 usage"):
            summary.summarize_record(name, record, record["case"], 1, review)

    def test_missing_response_metadata_cannot_silently_undercount(self):
        name = "baseline-current-v3-product-1.json"
        record = self.read(name)
        review = self.read("semantic-review.json")["reviews"][name]
        record["response_metadata"].pop()
        with self.assertRaisesRegex(summary.EvidenceError, "响应元数据与原始模型提议数量不一致"):
            summary.summarize_record(name, record, record["case"], 1, review)

    def test_initial_production_is_not_counted_as_current_provider_submission(self):
        name = "baseline-current-v3-production_budget_exhausted-1.json"
        record = self.read(name)
        review = self.read("semantic-review.json")["reviews"][name]
        self.assertEqual(record["snapshot"]["session"]["production"], 3)
        row = summary.summarize_record(name, record, record["case"], 1, review)
        self.assertEqual(row["actual_provider_submissions"], 0)
        record["snapshot"]["session"]["production"] = 4
        with self.assertRaisesRegex(summary.EvidenceError, "InitialProduction 后的提交数"):
            summary.summarize_record(name, record, record["case"], 1, review)

    def test_cli_never_overwrites_existing_evidence(self):
        original = (self.directory / "summary.json").read_bytes()
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(summary.main([str(self.directory), "--output", str(self.directory / "summary.json")]), 2)
        self.assertEqual((self.directory / "summary.json").read_bytes(), original)
        output = Path(self.temporary.name) / "new-summary.json"
        with contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(summary.main([str(self.directory), "--check-against", str(self.directory / "summary.json"),
                                           "--output", str(output)]), 0)
        self.assertEqual(json.loads(output.read_text()), self.read("summary.json"))


if __name__ == "__main__":
    unittest.main()
