import React, { useEffect, useRef } from 'react';
import { createChart, ColorType, type IChartApi, type ISeriesApi, type UTCTimestamp } from 'lightweight-charts';
import type { Candle } from '../types';

interface PriceChartProps {
  symbol: string;
  candles: Candle[];
  liveCandle?: Candle;
}

export const PriceChart: React.FC<PriceChartProps> = ({ symbol, candles, liveCandle }) => {
  const containerRef = useRef<HTMLDivElement | null>(null);
  const chartRef = useRef<IChartApi | null>(null);
  const seriesRef = useRef<ISeriesApi<'Candlestick'> | null>(null);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    const chart = createChart(el, {
      layout: {
        background: { type: ColorType.Solid, color: '#0d131f' },
        textColor: '#94a3b8',
      },
      grid: {
        vertLines: { color: '#1e293b' },
        horzLines: { color: '#1e293b' },
      },
      timeScale: {
        timeVisible: true,
        secondsVisible: false,
        borderColor: '#1e293b',
      },
      rightPriceScale: {
        borderColor: '#1e293b',
      },
      width: el.clientWidth || 640,
      height: 360,
    });

    const candleSeries = chart.addCandlestickSeries({
      upColor: '#10b981',
      downColor: '#ef4444',
      borderVisible: false,
      wickUpColor: '#10b981',
      wickDownColor: '#ef4444',
    });

    chartRef.current = chart;
    seriesRef.current = candleSeries;

    const onResize = () => {
      if (containerRef.current && chartRef.current) {
        chartRef.current.applyOptions({ width: containerRef.current.clientWidth });
      }
    };
    window.addEventListener('resize', onResize);

    return () => {
      window.removeEventListener('resize', onResize);
      chart.remove();
      chartRef.current = null;
      seriesRef.current = null;
    };
  }, []);

  useEffect(() => {
    if (!seriesRef.current || !candles.length) return;
    const formatted = candles.map((c) => ({
      time: c.bucket_start as UTCTimestamp,
      open: c.open / 100,
      high: c.high / 100,
      low: c.low / 100,
      close: c.close / 100,
    }));
    seriesRef.current.setData(formatted);
    chartRef.current?.timeScale().fitContent();
  }, [symbol, candles]);

  useEffect(() => {
    if (!seriesRef.current || !liveCandle || liveCandle.symbol !== symbol) return;
    seriesRef.current.update({
      time: liveCandle.bucket_start as UTCTimestamp,
      open: liveCandle.open / 100,
      high: liveCandle.high / 100,
      low: liveCandle.low / 100,
      close: liveCandle.close / 100,
    });
  }, [symbol, liveCandle]);

  return (
    <div className="panel chart-panel">
      <div className="panel-header">
        <span>{symbol} · 1m Candlesticks</span>
        <span className="muted">OHLCV</span>
      </div>
      <div ref={containerRef} className="chart-canvas" />
    </div>
  );
};
