import { beforeEach, describe, expect, it, vi } from 'vitest'

const { post, get } = vi.hoisted(() => ({
  post: vi.fn(),
  get: vi.fn()
}))

vi.mock('@/api/client', () => ({
  apiClient: { post, get }
}))

import {
  autoReauthorize,
  autoReauthorizeBatch,
  getAutoReauthorizeBatchJob,
  getAutoReauthorizeJob
} from '@/api/admin/accounts'

describe('admin account automatic re-authorization API', () => {
  beforeEach(() => {
    post.mockReset()
    get.mockReset()
  })

  it('calls the single-account automatic re-authorization endpoint', async () => {
    const job = { job_id: 'job-1', account_id: 42, status: 'running', logs: [] }
    post.mockResolvedValueOnce({ data: job })

    await expect(autoReauthorize(42)).resolves.toEqual(job)
    expect(post).toHaveBeenCalledWith('/admin/accounts/42/auto-reauthorize')
  })

  it('loads the automatic re-authorization job status and logs', async () => {
    const job = { job_id: 'job-1', account_id: 42, status: 'failed', logs: [{ message: '失败' }] }
    get.mockResolvedValueOnce({ data: job })

    await expect(getAutoReauthorizeJob(42, 'job-1')).resolves.toEqual(job)
    expect(get).toHaveBeenCalledWith('/admin/accounts/42/auto-reauthorize/job-1')
  })

  it('starts a serial batch automatic re-authorization job', async () => {
    const job = { job_id: 'batch-1', status: 'running', total: 2, completed: 0, results: [] }
    post.mockResolvedValueOnce({ data: job })

    await expect(autoReauthorizeBatch([11, 12])).resolves.toEqual(job)
    expect(post).toHaveBeenCalledWith('/admin/accounts/auto-reauthorize/batch', {
      account_ids: [11, 12]
    })
  })

  it('loads the batch automatic re-authorization progress and logs', async () => {
    const job = { job_id: 'batch-1', status: 'failed', total: 2, completed: 2, results: [] }
    get.mockResolvedValueOnce({ data: job })

    await expect(getAutoReauthorizeBatchJob('batch-1')).resolves.toEqual(job)
    expect(get).toHaveBeenCalledWith('/admin/accounts/auto-reauthorize/batch/batch-1')
  })
})
