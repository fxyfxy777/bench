import pandas as pd

df = pd.read_excel('/root/paddlejob/share-storage/gpfs/system-public/fanxiangyu/fxy/bench/results/fd_20260421_213659_最优/fd_bench_20260421_213659.xlsx')

print(f'Total experiments: {len(df)}')
print(f'Columns: {list(df.columns)}\n')

# 只看缓存相关列
cache_cols = [c for c in df.columns if 'Cache' in c or 'cached' in c.lower()]
print(f'Cache related columns: {cache_cols}\n')

# 查看有缓存指标的实验
if 'Mean Cached Tokens' in df.columns:
    df_sorted = df.sort_values('Mean Cached Tokens', ascending=False)
    print('=== Top 20 experiments by Mean Cached Tokens ===')
    for i, idx in enumerate(df_sorted.head(20).index):
        row = df_sorted.loc[idx]
        print(f'{i+1:2d}. {row["name"]:40s} | Cached: {row["Mean Cached Tokens"]:10.1f} | Input: {row["Mean Input Length"]:10.1f} | Output: {row["Mean Output Length"]:10.1f} | TTFT: {row["Mean TTFT"]:8.1f} | E2EL: {row["Median Session E2EL"]:10.1f} | TPOT: {row["Mean TPOT"]:8.1f} | QPS: {row.get("QPS", 0):8.1f}')

    # 统计缓存命中率范围
    cached_min = df['Mean Cached Tokens'].min()
    cached_max = df['Mean Cached Tokens'].max()
    cached_mean = df['Mean Cached Tokens'].mean()
    cached_median = df['Mean Cached Tokens'].median()
    print(f'\n=== Cached Tokens Statistics ===')
    print(f'Min: {cached_min}')
    print(f'Max: {cached_max}')
    print(f'Mean: {cached_mean:.2f}')
    print(f'Median: {cached_median:.2f}')

    # 显示前10名和后10名的对比
    print('\n=== Comparison: Top 10 vs Bottom 10 ===')
    top10 = df_sorted.head(10)
    bottom10 = df_sorted.tail(10)

    print(f'\nTop 10:')
    for col in ['Mean Cached Tokens', 'Mean TTFT', 'Mean TPOT', 'Median Session E2EL']:
        mean = top10[col].mean()
        print(f'  {col}: {mean:.2f}')

    print(f'\nBottom 10:')
    for col in ['Mean Cached Tokens', 'Mean TTFT', 'Mean TPOT', 'Median Session E2EL']:
        mean = bottom10[col].mean()
        print(f'  {col}: {mean:.2f}')
else:
    print('No Mean Cached Tokens column found')