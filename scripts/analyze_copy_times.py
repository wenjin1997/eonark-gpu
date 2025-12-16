#!/usr/bin/env python3
"""
分析 NVTX 事件中的 CopyToDevice 和 CopyFromDevice 时间统计
去除并行时间（重叠时间段）
"""

import sqlite3
import sys
from typing import List, Tuple
from collections import defaultdict


def merge_intervals(intervals: List[Tuple[int, int]]) -> List[Tuple[int, int]]:
    """
    合并重叠的区间，返回不重叠的区间列表
    """
    if not intervals:
        return []
    
    # 按开始时间排序
    sorted_intervals = sorted(intervals, key=lambda x: x[0])
    merged = [sorted_intervals[0]]
    
    for current in sorted_intervals[1:]:
        last = merged[-1]
        # 如果当前区间与上一个区间重叠或相邻，则合并
        if current[0] <= last[1]:
            merged[-1] = (last[0], max(last[1], current[1]))
        else:
            merged.append(current)
    
    return merged


def calculate_total_time(intervals: List[Tuple[int, int]]) -> int:
    """
    计算所有区间的总时间（去除重叠后）
    """
    merged = merge_intervals(intervals)
    return sum(end - start for start, end in merged)


def ns_to_ms(ns: int) -> float:
    """将纳秒转换为毫秒"""
    return ns / 1_000_000.0


def detect_prove_phases(cursor) -> Tuple[Tuple[int, int], Tuple[int, int]]:
    """
    检测 inner prove 和 outer prove 的时间范围
    通过查找 setupDevicePointers_G1Setup 事件来确定边界
    返回: ((inner_start, inner_end), (outer_start, outer_end))
    """
    # 查找所有 setupDevicePointers_G1Setup 事件
    cursor.execute("""
        SELECT start, end FROM NVTX_EVENTS 
        WHERE text LIKE '%setupDevicePointers_G1Setup%' 
        ORDER BY start
    """)
    setup_events = cursor.fetchall()
    
    if len(setup_events) < 2:
        # 如果没有找到两个阶段，返回整个时间范围
        cursor.execute("SELECT MIN(start), MAX(end) FROM NVTX_EVENTS")
        min_start, max_end = cursor.fetchone()
        return ((min_start, max_end), (min_start, max_end))
    
    inner_start = setup_events[0][0]  # 第一个 setupDevicePointers_G1Setup 的开始
    outer_start = setup_events[1][0]  # 第二个 setupDevicePointers_G1Setup 的开始
    
    # 查找第一个 prove 的结束时间（第一个阶段最后一个事件的结束时间）
    cursor.execute("""
        SELECT MAX(end) FROM NVTX_EVENTS 
        WHERE start >= ? AND end <= ?
    """, (inner_start, outer_start))
    inner_end_result = cursor.fetchone()
    inner_end = inner_end_result[0] if inner_end_result[0] else outer_start
    
    # 查找最后一个事件的结束时间作为 outer prove 的结束
    cursor.execute("SELECT MAX(end) FROM NVTX_EVENTS")
    outer_end = cursor.fetchone()[0]
    
    return ((inner_start, inner_end), (outer_start, outer_end))


def filter_events_by_phase(events: List[Tuple[Tuple[int, int], str]], 
                          phase_start: int, phase_end: int) -> List[Tuple[Tuple[int, int], str]]:
    """
    根据时间范围过滤事件
    事件只要与阶段时间范围有重叠就包含在内
    """
    filtered = []
    for (start, end), text in events:
        # 如果事件与阶段时间范围有重叠
        if not (end < phase_start or start > phase_end):
            filtered.append(((start, end), text))
    return filtered


def analyze_copy_times(sqlite_file: str):
    """
    分析 SQLite 文件中的 CopyToDevice 和 CopyFromDevice 时间
    """
    conn = sqlite3.connect(sqlite_file)
    cursor = conn.cursor()
    
    # 检测 inner 和 outer prove 的时间范围
    (inner_start, inner_end), (outer_start, outer_end) = detect_prove_phases(cursor)
    
    print(f"\n{'='*70}")
    print(f"检测到的 Prove 阶段时间范围:")
    print(f"{'='*70}")
    print(f"  Inner Prove:  {inner_start} - {inner_end} ({ns_to_ms(inner_end - inner_start):.2f} ms)")
    print(f"  Outer Prove:  {outer_start} - {outer_end} ({ns_to_ms(outer_end - outer_start):.2f} ms)")
    
    # 查询所有 CopyToDevice 和 CopyFromDevice 事件
    query = """
        SELECT start, end, text, color
        FROM NVTX_EVENTS
        WHERE (text LIKE '%CopyToDevice%' OR text LIKE '%CopyFromDevice%')
        ORDER BY start
    """
    
    cursor.execute(query)
    events = cursor.fetchall()
    
    # 分离 CopyToDevice 和 CopyFromDevice 事件
    copy_to_events = []
    copy_from_events = []
    
    # CopyToDevice 颜色: 0xFF00FF00 = 4278255360
    # CopyFromDevice 颜色: 0xFFFF0000 = 4294901760
    
    for start, end, text, color in events:
        interval = (start, end)
        if 'CopyToDevice' in text or color == 4278255360:
            copy_to_events.append((interval, text))
        elif 'CopyFromDevice' in text or color == 4294901760:
            copy_from_events.append((interval, text))
    
    # 按阶段分离事件
    inner_copy_to = filter_events_by_phase(copy_to_events, inner_start, inner_end)
    inner_copy_from = filter_events_by_phase(copy_from_events, inner_start, inner_end)
    outer_copy_to = filter_events_by_phase(copy_to_events, outer_start, outer_end)
    outer_copy_from = filter_events_by_phase(copy_from_events, outer_start, outer_end)
    
    # 计算统计信息
    def calculate_stats(events: List[Tuple[Tuple[int, int], str]], event_type: str):
        if not events:
            print(f"\n{event_type}: 无事件")
            return
        
        intervals = [interval for interval, _ in events]
        
        # 总时间（包括并行）
        total_time_ns = sum(end - start for start, end in intervals)
        
        # 去除并行后的时间
        merged_intervals = merge_intervals(intervals)
        non_overlapping_time_ns = sum(end - start for start, end in merged_intervals)
        
        # 并行时间
        parallel_time_ns = total_time_ns - non_overlapping_time_ns
        
        # 统计信息
        count = len(events)
        avg_time_ns = total_time_ns / count if count > 0 else 0
        
        # 计算最小、最大、中位数时间
        durations_ns = sorted([end - start for start, end in intervals])
        min_time_ns = durations_ns[0] if durations_ns else 0
        max_time_ns = durations_ns[-1] if durations_ns else 0
        median_time_ns = durations_ns[len(durations_ns) // 2] if durations_ns else 0
        
        print(f"\n{'='*70}")
        print(f"{event_type} 统计:")
        print(f"{'='*70}")
        print(f"  事件数量:                    {count:>6}")
        print(f"  总时间（包括并行）:          {ns_to_ms(total_time_ns):>12.6f} ms")
        print(f"  去除并行后的时间:            {ns_to_ms(non_overlapping_time_ns):>12.6f} ms")
        print(f"  并行时间:                    {ns_to_ms(parallel_time_ns):>12.6f} ms")
        if total_time_ns > 0:
            print(f"  并行比例:                    {parallel_time_ns / total_time_ns * 100:>11.2f}%")
        else:
            print(f"  并行比例:                    {0.0:>11.2f}%")
        print(f"\n  时间分布:")
        print(f"    最小时间:                  {ns_to_ms(min_time_ns):>12.6f} ms")
        print(f"    最大时间:                  {ns_to_ms(max_time_ns):>12.6f} ms")
        print(f"    平均时间:                  {ns_to_ms(avg_time_ns):>12.6f} ms")
        print(f"    中位数时间:                {ns_to_ms(median_time_ns):>12.6f} ms")
        
        # 显示前10个最长的事件
        sorted_events = sorted(events, key=lambda x: x[0][1] - x[0][0], reverse=True)
        print(f"\n  前10个最长的事件:")
        print(f"    {'序号':<4} {'事件名称':<55} {'耗时 (ms)':>12}")
        print(f"    {'-'*4} {'-'*55} {'-'*12}")
        for i, ((start, end), text) in enumerate(sorted_events[:10], 1):
            duration_ns = end - start
            print(f"    {i:<4} {text[:55]:<55} {ns_to_ms(duration_ns):>12.6f}")
    
    # 按阶段分析
    print(f"\n{'='*70}")
    print(f"INNER PROVE 阶段分析")
    print(f"{'='*70}")
    calculate_stats(inner_copy_to, "Inner Prove - CopyToDevice")
    calculate_stats(inner_copy_from, "Inner Prove - CopyFromDevice")
    inner_all_copy = inner_copy_to + inner_copy_from
    if inner_all_copy:
        inner_all_intervals = [interval for interval, _ in inner_all_copy]
        inner_total_ns = sum(end - start for start, end in inner_all_intervals)
        inner_merged = merge_intervals(inner_all_intervals)
        inner_non_overlapping_ns = sum(end - start for start, end in inner_merged)
        inner_parallel_ns = inner_total_ns - inner_non_overlapping_ns
        print(f"\n{'='*70}")
        print(f"Inner Prove - 总体 Copy 操作统计:")
        print(f"{'='*70}")
        print(f"  总事件数量:                  {len(inner_all_copy):>6}")
        print(f"  总时间（包括并行）:          {ns_to_ms(inner_total_ns):>12.6f} ms")
        print(f"  去除并行后的时间:            {ns_to_ms(inner_non_overlapping_ns):>12.6f} ms")
        print(f"  并行时间:                    {ns_to_ms(inner_parallel_ns):>12.6f} ms")
        if inner_total_ns > 0:
            print(f"  并行比例:                    {inner_parallel_ns / inner_total_ns * 100:>11.2f}%")
        else:
            print(f"  并行比例:                    {0.0:>11.2f}%")
    
    print(f"\n{'='*70}")
    print(f"OUTER PROVE 阶段分析")
    print(f"{'='*70}")
    calculate_stats(outer_copy_to, "Outer Prove - CopyToDevice")
    calculate_stats(outer_copy_from, "Outer Prove - CopyFromDevice")
    outer_all_copy = outer_copy_to + outer_copy_from
    if outer_all_copy:
        outer_all_intervals = [interval for interval, _ in outer_all_copy]
        outer_total_ns = sum(end - start for start, end in outer_all_intervals)
        outer_merged = merge_intervals(outer_all_intervals)
        outer_non_overlapping_ns = sum(end - start for start, end in outer_merged)
        outer_parallel_ns = outer_total_ns - outer_non_overlapping_ns
        print(f"\n{'='*70}")
        print(f"Outer Prove - 总体 Copy 操作统计:")
        print(f"{'='*70}")
        print(f"  总事件数量:                  {len(outer_all_copy):>6}")
        print(f"  总时间（包括并行）:          {ns_to_ms(outer_total_ns):>12.6f} ms")
        print(f"  去除并行后的时间:            {ns_to_ms(outer_non_overlapping_ns):>12.6f} ms")
        print(f"  并行时间:                    {ns_to_ms(outer_parallel_ns):>12.6f} ms")
        if outer_total_ns > 0:
            print(f"  并行比例:                    {outer_parallel_ns / outer_total_ns * 100:>11.2f}%")
        else:
            print(f"  并行比例:                    {0.0:>11.2f}%")
    
    # 计算总体统计（合并所有 Copy 事件）
    all_copy_events = copy_to_events + copy_from_events
    if all_copy_events:
        all_intervals = [interval for interval, _ in all_copy_events]
        total_all_ns = sum(end - start for start, end in all_intervals)
        merged_all = merge_intervals(all_intervals)
        non_overlapping_all_ns = sum(end - start for start, end in merged_all)
        parallel_all_ns = total_all_ns - non_overlapping_all_ns
        
        print(f"\n{'='*70}")
        print(f"总体 Copy 操作统计 (CopyToDevice + CopyFromDevice):")
        print(f"{'='*70}")
        print(f"  总事件数量:                  {len(all_copy_events):>6}")
        print(f"  总时间（包括并行）:          {ns_to_ms(total_all_ns):>12.6f} ms")
        print(f"  去除并行后的时间:            {ns_to_ms(non_overlapping_all_ns):>12.6f} ms")
        print(f"  并行时间:                    {ns_to_ms(parallel_all_ns):>12.6f} ms")
        if total_all_ns > 0:
            print(f"  并行比例:                    {parallel_all_ns / total_all_ns * 100:>11.2f}%")
        else:
            print(f"  并行比例:                    {0.0:>11.2f}%")
    
    conn.close()


def main():
    if len(sys.argv) < 2:
        print("用法: python3 analyze_copy_times.py <sqlite_file>")
        print("示例: python3 analyze_copy_times.py logs/gpu-4090/profiles/Test_Recursion_20251216_204241.sqlite")
        sys.exit(1)
    
    sqlite_file = sys.argv[1]
    try:
        analyze_copy_times(sqlite_file)
    except FileNotFoundError:
        print(f"错误: 文件不存在: {sqlite_file}")
        sys.exit(1)
    except sqlite3.Error as e:
        print(f"错误: SQLite 错误: {e}")
        sys.exit(1)
    except Exception as e:
        print(f"错误: {e}")
        import traceback
        traceback.print_exc()
        sys.exit(1)


if __name__ == "__main__":
    main()
