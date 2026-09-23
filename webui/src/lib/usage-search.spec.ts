import { describe, expect, it } from 'vitest'

import { groupMatches, groupNameMatches, matchingMembersOfGroup, membersOfGroup, userMatches } from './usage-search'
import type { AdminUsageEntryView } from '@/types/api'

/** entry builds a minimal AdminUsageEntryView — only id/groupName/groups matter to this module, the rest are never read. */
function entry(id: string, groupName?: string, groups?: string[]): AdminUsageEntryView {
  return {
    kind: groupName === undefined ? 'group' : 'user',
    id,
    groupName,
    groups,
    requestsPerMinute: 0,
    requestsPerDay: 0,
    tokensInPerDay: 0,
    tokensOutPerDay: 0,
    tokensInPerMonth: 0,
    tokensOutPerMonth: 0,
    costPerDayMicroUsd: 0,
    costPerMonthMicroUsd: 0,
  }
}

const alice = entry('alice', 'team-a')
const bob = entry('bob', 'team-a')
const carol = entry('carol', 'team-b')
const teamA = entry('team-a')
const teamB = entry('team-b')
const users = [alice, bob, carol]

describe('userMatches', () => {
  it('matches a case-insensitive substring of the id (query pre-lowercased by the caller)', () => {
    expect(userMatches(alice, 'lic')).toBe(true)
  })

  it('does not match an unrelated query', () => {
    expect(userMatches(alice, 'zzz')).toBe(false)
  })
})

describe('groupNameMatches', () => {
  it('matches on the group id alone, ignoring members entirely', () => {
    expect(groupNameMatches(teamA, 'team-a')).toBe(true)
  })

  it('does not match a query that only a member would match', () => {
    expect(groupNameMatches(teamA, 'alice')).toBe(false)
  })
})

describe('membersOfGroup', () => {
  it('joins users to a group on groupName === group.id', () => {
    expect(membersOfGroup(teamA, users)).toEqual([alice, bob])
    expect(membersOfGroup(teamB, users)).toEqual([carol])
  })

  // Multi-group support (review finding 4): a user's full membership lives
  // in `groups` (admin.go's adminUsageEntryView.Groups); groupName alone
  // only ever carries the FIRST one. Before this fix, membersOfGroup's
  // `groupName === group.id` join silently dropped a multi-group user from
  // every group but their first.
  it('joins a multi-group user into EVERY one of their member groups via `groups`, not just groupName', () => {
    const dave = entry('dave', 'team-a', ['team-a', 'team-b'])
    const roster = [alice, bob, carol, dave]

    expect(membersOfGroup(teamA, roster)).toEqual([alice, bob, dave])
    expect(membersOfGroup(teamB, roster)).toEqual([carol, dave])
  })

  it('falls back to [groupName] when `groups` is absent (a single-group user, or a fixture predating the field)', () => {
    expect(membersOfGroup(teamA, users)).toEqual([alice, bob])
  })
})

describe('matchingMembersOfGroup', () => {
  it('returns only the group members that themselves match query', () => {
    expect(matchingMembersOfGroup(teamA, users, 'alice')).toEqual([alice])
  })

  it('returns an empty array when no member matches', () => {
    expect(matchingMembersOfGroup(teamA, users, 'zzz')).toEqual([])
  })
})

describe('groupMatches', () => {
  it('matches via the group’s own name, with no matching member', () => {
    expect(groupMatches(teamA, users, 'team-a')).toBe(true)
  })

  it('matches via a member, with no name match', () => {
    expect(groupMatches(teamA, users, 'alice')).toBe(true)
  })

  it('does not match when neither the name nor any member matches', () => {
    expect(groupMatches(teamA, users, 'zzz')).toBe(false)
  })

  it('a group with no members only matches by name', () => {
    const lonely = entry('lonely-group')
    expect(groupMatches(lonely, users, 'lonely')).toBe(true)
    expect(groupMatches(lonely, users, 'alice')).toBe(false)
  })
})
