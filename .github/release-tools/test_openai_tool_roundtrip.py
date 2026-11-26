import copy
import argparse
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import MagicMock, patch
from urllib.error import HTTPError

import openai_tool_roundtrip as tools


def completed(output):
    return {'type': 'response.completed', 'response': {'status': 'completed', 'output': output}}


class ToolRoundtripTest(unittest.TestCase):
    def test_http_failure_records_status_without_key_or_private_response(self):
        with tempfile.TemporaryDirectory() as folder:
            args = argparse.Namespace(base_url='https://fixture.invalid/v1', model='fixture-model',
                                      proxy=None, output=str(Path(folder) / 'result.json'))
            opener = MagicMock()
            opener.open.side_effect = HTTPError('https://fixture.invalid', 403, 'Forbidden',
                                               {'server': 'fixture-edge'}, io.BytesIO(b'synthetic-secret'))
            with patch.object(tools, 'build_opener', return_value=opener), \
                    patch.object(tools.sys, 'stdin', io.StringIO('{"api_key":"synthetic-secret"}')), \
                    patch.object(tools.sys, 'stdout', io.StringIO()):
                with self.assertRaisesRegex(ValueError, 'tool_roundtrip_failed'):
                    tools.local_tool_smoke(args)
            result = json.loads(Path(args.output).read_text())
            self.assertEqual(len(result['turns']), 1)
            self.assertEqual(result['turns'][0]['http_status'], 403)
            self.assertEqual(result['turns'][0]['response_server'], 'fixture-edge')
            self.assertNotIn('synthetic-secret', Path(args.output).read_text())
            request = opener.open.call_args.args[0]
            self.assertEqual(request.get_header('User-agent'), 'RanSub2API-Tool-Verification/1.0')

    def responses(self):
        call = {'type': 'function_call', 'name': 'add_numbers', 'call_id': 'fixture-call',
                'arguments': '{"a":7,"b":11}'}
        first = [{'type': 'response.function_call_arguments.done', 'arguments': call['arguments']},
                 completed([{'type': 'reasoning', 'encrypted_content': 'fixture-encrypted-history'}, call])]
        final = [{'type': 'response.output_text.delta', 'delta': '18'},
                 completed([{'type': 'message', 'content': [{'type': 'output_text', 'text': '18'}]}])]
        return first, final

    def test_two_turns_preserve_history_call_id_model_and_session(self):
        first, final = self.responses()
        payloads = []
        def send(payload):
            payloads.append(copy.deepcopy(payload))
            return first if len(payloads) == 1 else final
        result = tools.exercise(send, 'fixture-model', 'fixture-session')
        self.assertEqual(len(payloads), 2)
        self.assertTrue(result['final_answer_verified'])
        self.assertFalse(result['monitoring_evidence_promoted'])
        self.assertEqual(payloads[0]['model'], payloads[1]['model'])
        self.assertEqual(payloads[0]['prompt_cache_key'], payloads[1]['prompt_cache_key'])
        self.assertEqual(payloads[1]['input'][1], first[-1]['response']['output'][0])
        output = payloads[1]['input'][-1]
        self.assertEqual(output['type'], 'function_call_output')
        self.assertEqual(output['call_id'], 'fixture-call')
        self.assertEqual(json.loads(output['output']), {'sum': 18})
        self.assertEqual(payloads[1]['tool_choice'], 'none')

    def test_text_only_success_does_not_prove_tool_support(self):
        calls = []
        _, text = self.responses()
        with self.assertRaisesRegex(ValueError, 'expected_one_function_call'):
            tools.exercise(lambda payload: calls.append(payload) or text, 'fixture-model', 'fixture-session')
        self.assertEqual(len(calls), 1)

    def test_incomplete_function_arguments_never_trigger_second_request(self):
        first, _ = self.responses()
        calls = []
        with self.assertRaisesRegex(ValueError, 'tool_arguments_stream_not_completed'):
            tools.exercise(lambda payload: calls.append(payload) or first[1:], 'fixture-model', 'fixture-session')
        self.assertEqual(len(calls), 1)

    def test_wrong_arguments_never_execute_a_tool(self):
        first, _ = self.responses()
        first[-1]['response']['output'][-1]['arguments'] = '{"a":true,"b":11}'
        with self.assertRaisesRegex(ValueError, 'unexpected_function_arguments'):
            tools.exercise(lambda payload: first, 'fixture-model', 'fixture-session')

    def test_failed_incomplete_missing_and_duplicate_terminal_are_rejected(self):
        good = completed([])
        for events in [[], [good, good], [good, {'type': 'response.failed'}],
                       [good, {'type': 'error'}], [good, {'type': 'response.incomplete'}]]:
            with self.subTest(events=events), self.assertRaises(ValueError):
                tools.completed_response(events)

    def test_missing_or_wrong_final_text_is_not_success(self):
        for final in [[completed([])], [{'type': 'response.output_text.delta', 'delta': '19'},
                       completed([{'type': 'message', 'content': [{'type': 'output_text', 'text': '19'}]}])]]:
            first, _ = self.responses()
            replies = iter([first, final])
            with self.subTest(final=final), self.assertRaisesRegex(ValueError, 'incorrect_final_tool_result'):
                tools.exercise(lambda payload: next(replies), 'fixture-model', 'fixture-session')

    def test_sse_parser_handles_comments_multiline_and_done_without_faking_completion(self):
        lines = [b': heartbeat\n', b'\n', b'data: {"type":\n', b'data: "response.created"}\n',
                 b'\n', b'data: [DONE]\n', b'\n']
        self.assertEqual(list(tools.sse_events(lines)), [{'type': 'response.created'}])
        with self.assertRaisesRegex(ValueError, 'truncated_sse_event'):
            list(tools.sse_events([b'data: {"type":"response.completed"}\n']))

    def test_heartbeats_do_not_extend_the_probe_deadline(self):
        with patch.object(tools.time, 'monotonic', return_value=91), \
                self.assertRaisesRegex(ValueError, 'sse_request_deadline_exceeded'):
            list(tools.sse_events([b': heartbeat\n'], deadline=90))


if __name__ == '__main__':
    unittest.main()
