"""One bounded, explicit Responses tool roundtrip; never a scheduling signal."""
import json
import re
import sys
import time
import uuid
from pathlib import Path
from urllib.error import HTTPError
from urllib.request import ProxyHandler, Request, build_opener
from urllib.parse import urlsplit


def require(condition, reason):
    if not condition:
        raise ValueError(reason)


def sse_events(lines, deadline=None):
    data = []
    for raw in lines:
        require(deadline is None or time.monotonic() < deadline, 'sse_request_deadline_exceeded')
        line = raw.decode('utf-8').rstrip('\r\n')
        if line.startswith('data:'):
            data.append(line[5:].lstrip(' '))
        elif not line and data:
            value = '\n'.join(data)
            data = []
            if value != '[DONE]':
                event = json.loads(value)
                require(isinstance(event, dict), 'invalid_sse_event')
                yield event
    require(not data, 'truncated_sse_event')


def completed_response(events):
    require(not any(event.get('type') in ('error', 'response.failed', 'response.incomplete')
                    for event in events), 'failed_or_incomplete_response')
    completed = [event for event in events if event.get('type') == 'response.completed']
    require(len(completed) == 1, 'expected_one_completed_event')
    response = completed[0].get('response')
    require(isinstance(response, dict) and response.get('status') == 'completed', 'invalid_completed_response')
    require(isinstance(response.get('output'), list), 'missing_response_output')
    require(all(isinstance(item, dict) for item in response['output']), 'invalid_response_output')
    return response


def exercise(send, model, session):
    tool = {'type': 'function', 'name': 'add_numbers', 'description': 'Add two integers.', 'strict': True,
            'parameters': {'type': 'object', 'properties': {'a': {'type': 'integer', 'enum': [7]},
              'b': {'type': 'integer', 'enum': [11]}}, 'required': ['a', 'b'], 'additionalProperties': False}}
    question = {'role': 'user', 'content': [{'type': 'input_text',
        'text': 'Use add_numbers to compute 7 + 11. After its result, answer with only the integer 18.'}]}
    payload = {'model': model, 'input': [question], 'tools': [tool],
               'tool_choice': {'type': 'function', 'name': 'add_numbers'},
               'parallel_tool_calls': False, 'stream': True, 'store': False,
               'max_output_tokens': 512, 'reasoning': {'effort': 'low'},
               'include': ['reasoning.encrypted_content'], 'prompt_cache_key': session}
    first_events = send(payload)
    first = completed_response(first_events)
    calls = [item for item in first['output'] if item.get('type') == 'function_call']
    require(len(calls) == 1, 'expected_one_function_call')
    call = calls[0]
    require(call.get('name') == 'add_numbers' and isinstance(call.get('call_id'), str)
            and bool(call['call_id']), 'unexpected_function_call')
    arguments = json.loads(call.get('arguments', ''))
    require(isinstance(arguments, dict) and set(arguments) == {'a', 'b'} and
            type(arguments['a']) is int and type(arguments['b']) is int and
            arguments == {'a': 7, 'b': 11}, 'unexpected_function_arguments')
    argument_events = [event for event in first_events
                       if event.get('type') == 'response.function_call_arguments.done']
    require(len(argument_events) == 1 and json.loads(argument_events[0].get('arguments', '')) == arguments,
            'tool_arguments_stream_not_completed')
    followup = dict(payload, tool_choice='none', input=[question, *first['output'],
        {'type': 'function_call_output', 'call_id': call['call_id'],
         'output': json.dumps({'sum': arguments['a'] + arguments['b']})}])
    final_events = send(followup)
    final = completed_response(final_events)
    require(not any(item.get('type') == 'function_call' for item in final['output']), 'unexpected_extra_tool_call')
    text = ''.join(part.get('text', '') for item in final['output'] if item.get('type') == 'message'
                   for part in item.get('content', []) if part.get('type') == 'output_text')
    require(text.strip() == '18', 'incorrect_final_tool_result')
    require(any(event.get('type') == 'response.output_text.delta' and event.get('delta')
                for event in final_events), 'missing_final_text_delta')
    return {'tool_name': 'add_numbers', 'tool_arguments_verified': True,
            'function_call_output_returned': True, 'final_answer_verified': True,
            'completed_turns': 2, 'monitoring_evidence_promoted': False}


def local_tool_smoke(args):
    parsed = urlsplit(args.base_url)
    require(parsed.scheme in ('http', 'https') and parsed.hostname and not parsed.username
            and not parsed.password and not parsed.query and not parsed.fragment, 'invalid_base_url')
    require(args.model and len(args.model) <= 200, 'invalid_model')
    if args.proxy:
        proxy = urlsplit(args.proxy)
        require(proxy.scheme in ('http', 'https') and proxy.hostname and not proxy.username
                and not proxy.password, 'invalid_proxy_url')
    supplied = json.load(sys.stdin)
    key = supplied.get('api_key')
    require(isinstance(key, str) and key.strip() and '\r' not in key and '\n' not in key, 'invalid_api_key')
    opener = build_opener(ProxyHandler({'http': args.proxy, 'https': args.proxy} if args.proxy else {}))
    session = 'tool-smoke-' + str(uuid.uuid4())
    result = {'passed': False, 'model': args.model, 'scope': 'responses_http_tool_roundtrip',
              'automatic_monitoring_changed': False, 'turns': []}

    def send(payload):
        require(len(result['turns']) < 2, 'physical_request_budget_exceeded')
        turn = {'client_request_id': str(uuid.uuid4()), 'completed_events': 0}
        result['turns'].append(turn)
        headers = {'Authorization': 'Bearer ' + key, 'Content-Type': 'application/json',
                   'Accept': 'text/event-stream', 'Session-Id': session,
                   'User-Agent': 'RanSub2API-Tool-Verification/1.0',
                   'X-Client-Request-Id': turn['client_request_id']}
        request = Request(args.base_url.rstrip('/') + '/responses', method='POST',
                          headers=headers, data=json.dumps(payload).encode())
        start = time.monotonic()
        try:
            with opener.open(request, timeout=90) as response:
                turn['http_status'] = response.status
                actual = response.headers.get('x-client-request-id', '')
                require(re.fullmatch(r'[A-Za-z0-9_-]{1,255}', actual), 'missing_server_request_id')
                turn['client_request_id'] = actual
                events = list(sse_events(response, deadline=start + 90))
        except HTTPError as error:
            turn['http_status'] = error.code
            turn['response_server'] = error.headers.get('server', '')
            error.close()
            raise
        turn['seconds'] = round(time.monotonic() - start, 3)
        turn['event_types'] = sorted({event.get('type', '') for event in events})
        turn['completed_events'] = sum(event.get('type') == 'response.completed' for event in events)
        completed = completed_response(events)
        # Keep only upstream-reported counters, never tool transcripts or encrypted history.
        usage = completed.get('usage')
        turn['upstream_usage'] = {name: usage[name] for name in ('input_tokens', 'output_tokens', 'total_tokens')
                                  if name in usage} if isinstance(usage, dict) else None
        return events

    try:
        result.update(exercise(send, args.model, session), passed=True)
    except (OSError, ValueError, TypeError, KeyError) as error:
        result['error'] = str(error) if type(error) is ValueError else type(error).__name__
    finally:
        Path(args.output).write_text(json.dumps(result, indent=2) + '\n', encoding='utf-8')
    print(json.dumps(result))
    require(result['passed'], 'tool_roundtrip_failed; inspect result before sending another paid request')
