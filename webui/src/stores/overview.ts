import { defineStore } from 'pinia'

import { adminFetch, isAuthRejection, messageOf } from '@/lib/api'
import { createVisibilityPoller } from '@/lib/polling'
import type { VisibilityPoller } from '@/lib/polling'
import { seriesUrl } from '@/lib/range'
import { TOP_USERS_METRICS_BY_MODE, topUsersCapCheckResult, topUsersTotalsUrl } from '@/lib/top-users'
import type { Series } from '@/lib/series'
import { useAuthStore } from '@/stores/auth'
import { useDashboardStore } from '@/stores/dashboard'
import { useFiltersStore } from '@/stores/filters'
import type { AdminSeriesResponse, AdminTotalsResponse, AdminUsageModelsResponse, HistoryMetric, HistoryWindow } from '@/types/api'

const REFRESH_MS = 60_000
const TOP_N = 5
const TOTAL_SCOPE = 'total'
const TOTAL_METRICS = ['req', 'tokin', 'tokout', 'cost'] as const
const COMPARED_METRICS = ['req', 'tokin', 'cost'] as const

type TotalMetric = (typeof TOTAL_METRICS)[number]
type Points = Record<TotalMetric, number[]>

const emptyPoints = (): Points => ({ req: [], tokin: [], tokout: [], cost: [] })
const emptyError = () => ({ traffic: '', models: '', users: '' })

export const RANGE_TOO_LARGE = 'Too many configured users to rank for this range. Pick a shorter range.'

export const useOverviewStore = defineStore('overview', {
  state: () => ({
    buckets: [] as string[],
    totals: emptyPoints(),
    previous: null as Pick<Points, 'req' | 'tokin' | 'cost'> | null,
    topModelSeries: [] as Series[],
    topModelsByCost: [] as AdminUsageModelsResponse['models'],
    topUserRows: [] as AdminTotalsResponse['rows'],
    trafficLoaded: false,
    modelsLoaded: false,
    usersLoaded: false,
    error: emptyError(),
    lastUpdated: null as Date | null,
    fetchedFor: '',
    reqId: 0,
    poller: undefined as VisibilityPoller | undefined,
  }),
  actions: {
    async refresh(): Promise<void> {
      if (!useAuthStore().isAuthenticated) return
      const filters = useFiltersStore()
      const key = `${filters.window}:${filters.span}:${filters.cmpSpec.requested}`
      if (key !== this.fetchedFor) this.clearData()
      this.fetchedFor = key
      const id = ++this.reqId
      const { window, span } = filters
      const offset = filters.cmpSpec.requested ? filters.cmpSpec.offset : 0
      await Promise.all([this.loadTraffic(id, window, span, offset), this.loadModels(id, window, span), this.loadUsers(id, window, span)])
      if (id === this.reqId) this.lastUpdated = new Date()
    },
    clearData(): void {
      this.buckets = []
      this.totals = emptyPoints()
      this.previous = null
      this.topModelSeries = []
      this.topModelsByCost = []
      this.topUserRows = []
      this.trafficLoaded = this.modelsLoaded = this.usersLoaded = false
      this.error = emptyError()
    },
    async series(scope: string[], metric: HistoryMetric, window: HistoryWindow, span: number, offset: number): Promise<AdminSeriesResponse> {
      return adminFetch<AdminSeriesResponse>(seriesUrl({ scope, metric, window, span, offset: offset || undefined }))
    },
    async loadTraffic(id: number, window: HistoryWindow, span: number, offset: number): Promise<void> {
      try {
        const totalOf = async (metric: HistoryMetric, off: number) => {
          const res = await this.series([TOTAL_SCOPE], metric, window, span, off)
          return { buckets: res.buckets, points: res.series.find((s) => s.scope === TOTAL_SCOPE)?.points ?? [] }
        }
        const current = await Promise.all(TOTAL_METRICS.map((m) => totalOf(m, 0)))
        const prior = offset > 0 ? await Promise.all(COMPARED_METRICS.map((m) => totalOf(m, offset))) : null
        const ranking = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=req&window=${window}&span=${span}&limit=${TOP_N}`,
        )
        const scopes = ranking.models.map((m) => `model:${m.id}`)
        const top = scopes.length > 0 ? (await this.series(scopes, 'req', window, span, 0)).series : []
        if (id !== this.reqId) return
        this.buckets = current[0]?.buckets ?? []
        this.totals = { req: current[0]!.points, tokin: current[1]!.points, tokout: current[2]!.points, cost: current[3]!.points }
        this.previous = prior ? { req: prior[0]!.points, tokin: prior[1]!.points, cost: prior[2]!.points } : null
        this.topModelSeries = top
        this.trafficLoaded = true
        this.error.traffic = ''
      } catch (err) {
        if (id === this.reqId && !isAuthRejection(err)) this.error.traffic = messageOf(err)
      }
    },
    async loadModels(id: number, window: HistoryWindow, span: number): Promise<void> {
      try {
        const res = await adminFetch<AdminUsageModelsResponse>(
          `/admin/api/usage/models?metric=cost&window=${window}&span=${span}&limit=${TOP_N}`,
        )
        if (id !== this.reqId) return
        this.topModelsByCost = res.models
        this.modelsLoaded = true
        this.error.models = ''
      } catch (err) {
        if (id === this.reqId && !isAuthRejection(err)) this.error.models = messageOf(err)
      }
    },
    async loadUsers(id: number, window: HistoryWindow, span: number): Promise<void> {
      const configured = useDashboardStore().usage?.users.length ?? null
      if (topUsersCapCheckResult(configured, TOP_USERS_METRICS_BY_MODE.req.length, span)) {
        this.topUserRows = []
        this.usersLoaded = false
        this.error.users = RANGE_TOO_LARGE
        return
      }
      try {
        const res = await adminFetch<AdminTotalsResponse>(topUsersTotalsUrl(window, span, 'req'))
        if (id !== this.reqId) return
        this.topUserRows = res.rows
        this.usersLoaded = true
        this.error.users = ''
      } catch (err) {
        if (id === this.reqId && !isAuthRejection(err)) this.error.users = messageOf(err)
      }
    },
    startPolling(): void {
      if (this.poller) return
      this.poller = createVisibilityPoller({ intervalMs: REFRESH_MS, tick: () => this.refresh() })
      this.poller.start()
    },
    stopPolling(): void {
      this.poller?.stop()
      this.poller = undefined
    },
  },
})
