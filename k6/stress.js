import http from 'k6/http';
import { check, sleep } from 'k6';
import { SharedArray } from 'k6/data';
import { Counter, Rate, Trend } from 'k6/metrics';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const USERNAME = __ENV.K6_USERNAME || 'admin';
const PASSWORD = __ENV.K6_PASSWORD || 'abcdefghijkl';
const POLL_INTERVAL_SEC = Number(__ENV.K6_POLL_INTERVAL_SEC || '0.25');
const POLL_TIMEOUT_SEC = Number(__ENV.K6_POLL_TIMEOUT_SEC || '60');
const MANIFEST_PATH = __ENV.K6_MANIFEST_PATH || './migrations.json';

const manifest = new SharedArray('approved-migrations', () => {
  const parsed = JSON.parse(open(MANIFEST_PATH));
  return [parsed];
});

const apiLatencyMs = new Trend('api_response_time_ms', true);
const apiErrorRate = new Rate('api_error_rate');
const queueWaitMs = new Trend('queue_wait_ms', true);
const jobDurationMs = new Trend('job_completion_duration_ms', true);
const jobSuccessRate = new Rate('job_execution_success_rate');
const jobsCompleted = new Counter('jobs_completed_total');
const jobsSubmitted = new Counter('jobs_submitted_total');

export const options = {
  scenarios: {
    api_read_access: {
      executor: 'ramping-vus',
      exec: 'apiReadWorkload',
      startVUs: 2,
      stages: [
        { duration: '20s', target: 10 }, // normal load
        { duration: '30s', target: 25 }, // above normal
        { duration: '30s', target: 45 }, // peak stress
        { duration: '30s', target: 10 }, // recovery to normal
        { duration: '15s', target: 0 },  // cooldown
      ],
      gracefulRampDown: '10s',
    },
    migration_same_db: {
      executor: 'ramping-vus',
      exec: 'submitSameDBMigration',
      startVUs: 1,
      stages: [
        { duration: '20s', target: 2 },
        { duration: '30s', target: 5 },
        { duration: '30s', target: 8 },
        { duration: '30s', target: 2 },
        { duration: '15s', target: 0 },
      ],
      gracefulRampDown: '15s',
    },
    migration_diff_db: {
      executor: 'ramping-vus',
      exec: 'submitDifferentDBMigration',
      startVUs: 1,
      stages: [
        { duration: '20s', target: 2 },
        { duration: '30s', target: 4 },
        { duration: '30s', target: 6 },
        { duration: '30s', target: 2 },
        { duration: '15s', target: 0 },
      ],
      gracefulRampDown: '15s',
    },
  },
  thresholds: {
    http_req_duration: ['p(95)<1500'],
    http_req_failed: ['rate<0.05'],
    api_response_time_ms: ['p(95)<1500'],
    api_error_rate: ['rate<0.05'],
    job_execution_success_rate: ['rate>0.95'],
  },
};

const vuSession = {
  authenticated: false,
  csrf: '',
};

function ensureLogin() {
  if (vuSession.authenticated) return;

  const csrfRes = http.get(`${BASE_URL}/api/v1/auth/login-csrf`, {
    tags: { endpoint: 'login_csrf' },
  });
  apiLatencyMs.add(csrfRes.timings.duration);
  const csrfToken = csrfRes.json('data.csrf_token');
  const csrfOk = check(csrfRes, {
    'login-csrf status 200': r => r.status === 200 && Boolean(csrfToken),
  });
  if (!csrfOk) {
    apiErrorRate.add(true);
    return;
  }

  const loginRes = http.post(
    `${BASE_URL}/api/v1/auth/login`,
    JSON.stringify({ username: USERNAME, password: PASSWORD }),
    {
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': csrfToken,
      },
      tags: { endpoint: 'login' },
    },
  );
  apiLatencyMs.add(loginRes.timings.duration);
  if (!check(loginRes, { 'login status 200': r => r.status === 200 })) {
    apiErrorRate.add(true);
    return;
  }

  const sessRes = http.get(`${BASE_URL}/api/v1/auth/session`, {
    tags: { endpoint: 'session' },
  });
  apiLatencyMs.add(sessRes.timings.duration);
  const sessionCSRF = sessRes.json('data.csrf_token');
  if (check(sessRes, { 'session status 200': r => r.status === 200 && Boolean(sessionCSRF) })) {
    vuSession.authenticated = true;
    vuSession.csrf = sessionCSRF;
    apiErrorRate.add(false);
  } else {
    apiErrorRate.add(true);
  }
}

function recordAPI(res, okCheck) {
  apiLatencyMs.add(res.timings.duration);
  apiErrorRate.add(!okCheck);
}

export function apiReadWorkload() {
  ensureLogin();
  if (!vuSession.authenticated) {
    sleep(1);
    return;
  }

  const dbsRes = http.get(`${BASE_URL}/api/v1/databases`, {
    tags: { endpoint: 'list_databases' },
  });
  const dbs = dbsRes.json('data') || [];
  recordAPI(dbsRes, check(dbsRes, { 'list databases 200': r => r.status === 200 }));

  if (dbs.length > 0) {
    const db = dbs[exec.scenario.iterationInTest % dbs.length];
    const browseRes = http.get(`${BASE_URL}/api/v1/databases/${db.id}`, {
      tags: { endpoint: 'browse_database' },
    });
    recordAPI(browseRes, check(browseRes, { 'browse db 200': r => r.status === 200 }));

    const histRes = http.get(`${BASE_URL}/api/v1/databases/${db.id}/history`, {
      tags: { endpoint: 'list_history' },
    });
    recordAPI(histRes, check(histRes, { 'history 200': r => r.status === 200 }));

    const queryRes = http.post(
      `${BASE_URL}/api/v1/databases/${db.id}/query`,
      JSON.stringify({ sql: 'SELECT 1 AS ok' }),
      {
        headers: {
          'Content-Type': 'application/json',
          'X-CSRF-Token': vuSession.csrf,
        },
        tags: { endpoint: 'sql_query' },
      },
    );
    recordAPI(queryRes, check(queryRes, { 'sql query 200': r => r.status === 200 }));
  }

  sleep(0.3);
}

function submitAndPollJob(item, scenarioTag) {
  ensureLogin();
  if (!vuSession.authenticated || !item) {
    sleep(1);
    return;
  }

  const submitStart = Date.now();
  const submitRes = http.post(
    `${BASE_URL}/api/v1/databases/${item.database_id}/executions`,
    JSON.stringify({
      migration_id: item.migration_id,
      revision_id: item.revision_id,
      approval_request_id: item.approval_request_id,
      action: item.action,
      mode: item.mode,
    }),
    {
      headers: {
        'Content-Type': 'application/json',
        'X-CSRF-Token': vuSession.csrf,
      },
      tags: { endpoint: 'submit_execution', scenario_type: scenarioTag },
    },
  );

  // 202 Accepted means the job was queued — NOT that the migration succeeded.
  const accepted = check(submitRes, {
    'submit accepted (202)': r => r.status === 202,
  });
  recordAPI(submitRes, accepted);

  if (!accepted) {
    return;
  }

  const jobID = submitRes.json('data.id');
  if (!jobID) {
    jobSuccessRate.add(false);
    return;
  }
  jobsSubmitted.add(1);

  let recordedQueueWait = false;
  const deadline = submitStart + POLL_TIMEOUT_SEC * 1000;

  while (Date.now() < deadline) {
    sleep(POLL_INTERVAL_SEC);
    const pollRes = http.get(`${BASE_URL}/api/v1/jobs/${encodeURIComponent(jobID)}`, {
      tags: { endpoint: 'poll_job', scenario_type: scenarioTag },
    });
    const pollOk = check(pollRes, { 'poll job 200': r => r.status === 200 });
    recordAPI(pollRes, pollOk);
    if (!pollOk) continue;

    const job = pollRes.json('data');
    if (!job) continue;

    if (!recordedQueueWait && job.status !== 'QUEUED') {
      queueWaitMs.add(Date.now() - submitStart);
      recordedQueueWait = true;
    }

    if (job.status === 'SUCCEEDED' || job.status === 'FAILED' || job.status === 'CANCELLED') {
      jobDurationMs.add(Date.now() - submitStart);
      jobsCompleted.add(1);
      jobSuccessRate.add(job.status === 'SUCCEEDED');
      return;
    }
  }

  jobDurationMs.add(Date.now() - submitStart);
  jobsCompleted.add(1);
  jobSuccessRate.add(false);
}

export function submitSameDBMigration() {
  const pool = manifest[0]?.databases?.shop || [];
  const idx = exec.scenario.iterationInTest;
  if (idx >= pool.length) {
    sleep(1);
    return;
  }
  submitAndPollJob(pool[idx], 'same_db');
}

export function submitDifferentDBMigration() {
  const shopPool = manifest[0]?.databases?.shop || [];
  const analyticsPool = manifest[0]?.databases?.analytics || [];
  const iter = exec.scenario.iterationInTest;
  const useAnalytics = iter % 2 === 0;
  const item = useAnalytics
    ? analyticsPool[Math.floor(iter / 2)]
    : shopPool[shopPool.length - 1 - Math.floor(iter / 2)];
  if (!item) {
    sleep(1);
    return;
  }
  submitAndPollJob(item, 'diff_db');
}

export function handleSummary(data) {
  const dur = data.metrics.http_req_duration?.values || {};
  const reqs = data.metrics.http_reqs?.values || {};
  const failed = data.metrics.http_req_failed?.values || {};
  const qWait = data.metrics.queue_wait_ms?.values || {};
  const jDur = data.metrics.job_completion_duration_ms?.values || {};
  const jComp = data.metrics.jobs_completed_total?.values || {};
  const jSucc = data.metrics.job_execution_success_rate?.values || {};
  const testSec = (data.state?.testRunDurationMs || 1) / 1000;

  const summary = {
    api_response_time_ms: { p50: dur.med, p95: dur['p(95)'], avg: dur.avg, max: dur.max },
    request_throughput_rps: reqs.rate,
    total_requests: reqs.count,
    http_error_rate: failed.rate,
    queue_wait_ms: { p50: qWait.med, p95: qWait['p(95)'], avg: qWait.avg, max: qWait.max },
    job_completion_duration_ms: { p50: jDur.med, p95: jDur['p(95)'], avg: jDur.avg, max: jDur.max },
    jobs_completed: jComp.count || 0,
    job_completion_throughput_per_sec: (jComp.count || 0) / testSec,
    job_execution_success_rate: jSucc.rate,
  };

  const lines = [
    '=== DBMP k6 Stress Test Summary ===',
    `API Response Time : p50=${(summary.api_response_time_ms.p50 || 0).toFixed(2)}ms  p95=${(summary.api_response_time_ms.p95 || 0).toFixed(2)}ms`,
    `Request Throughput: ${(summary.request_throughput_rps || 0).toFixed(2)} req/s (${summary.total_requests || 0} requests)`,
    `HTTP Error Rate   : ${((summary.http_error_rate || 0) * 100).toFixed(2)}%`,
    `Queue Wait Time   : p50=${(summary.queue_wait_ms.p50 || 0).toFixed(2)}ms  p95=${(summary.queue_wait_ms.p95 || 0).toFixed(2)}ms`,
    `Job Duration      : p50=${(summary.job_completion_duration_ms.p50 || 0).toFixed(2)}ms  p95=${(summary.job_completion_duration_ms.p95 || 0).toFixed(2)}ms`,
    `Job Throughput    : ${summary.job_completion_throughput_per_sec.toFixed(2)} jobs/s (${summary.jobs_completed} completed)`,
    `Job Success Rate  : ${((summary.job_execution_success_rate || 0) * 100).toFixed(2)}%`,
    '',
  ];

  return {
    stdout: lines.join('\n'),
    'k6/summary.json': JSON.stringify(summary, null, 2),
  };
}
