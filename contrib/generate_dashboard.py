#!/usr/bin/env python3
"""
Kubernetes Workload Metrics Dashboard Generator

This script generates an HTML dashboard from JSON metrics files that can be deployed on GitHub Pages.
It focuses on podreadylatency as the main KPI and OVN container CPU/Memory usage as secondary metrics.
"""

import json
import os
import sys
from datetime import datetime
from typing import Dict, List, Any, Optional
import argparse


class MetricsProcessor:
    """Process and analyze metrics data from JSON files."""
    
    def __init__(self, metrics_dir: str = "."):
        self.metrics_dir = metrics_dir
        self.pod_latency_file = "podLatencyMeasurement-kubelet-density-cni.json"
        self.pod_cpu_file = "podCPU.json"
        self.pod_memory_file = "podMemory.json"
        
    def load_json_file(self, filename: str) -> List[Dict[str, Any]]:
        """Load and parse JSON file."""
        filepath = os.path.join(self.metrics_dir, filename)
        try:
            with open(filepath, 'r') as f:
                data = json.load(f)
                print(f"✓ Loaded {len(data)} records from {filename}")
                return data
        except FileNotFoundError:
            print(f"✗ Error: {filename} not found in {self.metrics_dir}")
            return []
        except json.JSONDecodeError as e:
            print(f"✗ Error parsing {filename}: {e}")
            return []
    
    def process_pod_latency_data(self, data: List[Dict[str, Any]]) -> Dict[str, Any]:
        """Process pod latency data and calculate statistics."""
        valid_data = [
            d for d in data 
            if d.get('podReadyLatency') is not None and d.get('timestamp')
        ]
        
        if not valid_data:
            return {"data": [], "stats": {}}
        
        # Sort by timestamp
        valid_data.sort(key=lambda x: x['timestamp'])
        
        # Calculate statistics
        latencies = [d['podReadyLatency'] for d in valid_data]
        stats = {
            "total_pods": len(valid_data),
            "avg_latency": sum(latencies) / len(latencies),
            "max_latency": max(latencies),
            "min_latency": min(latencies),
            "start_time": valid_data[0]['timestamp'],
            "end_time": valid_data[-1]['timestamp']
        }
        
        return {
            "data": valid_data,
            "stats": stats
        }
    
    def process_ovn_data(self, data: List[Dict[str, Any]], metric_type: str) -> Dict[str, List[Dict[str, Any]]]:
        """Process OVN container CPU/Memory data."""
        ovn_data = {}
        
        for record in data:
            labels = record.get('labels', {})
            pod_name = labels.get('pod', '')
            
            # Filter for OVN containers
            if not any(keyword in pod_name for keyword in ['ovnkube-', 'ovs-']):
                continue
                
            # Categorize container type
            container_type = self.get_container_type(pod_name)
            
            if container_type not in ovn_data:
                ovn_data[container_type] = []
            
            # Convert memory to MB if needed
            value = record.get('value', 0)
            if metric_type == 'memory':
                value = value / (1024 * 1024)  # Convert bytes to MB
            
            ovn_data[container_type].append({
                "timestamp": record.get('timestamp'),
                "value": value,
                "pod": pod_name,
                "node": labels.get('node', 'unknown')
            })
        
        # Sort each container type by timestamp
        for container_type in ovn_data:
            ovn_data[container_type].sort(key=lambda x: x['timestamp'])
        
        return ovn_data
    
    def get_container_type(self, pod_name: str) -> str:
        """Categorize OVN container types."""
        if 'ovnkube-node' in pod_name:
            return 'OVN Node'
        elif 'ovnkube-control-plane' in pod_name:
            return 'OVN Control Plane'
        elif 'ovnkube-identity' in pod_name:
            return 'OVN Identity'
        elif 'ovs-node' in pod_name:
            return 'OVS Node'
        else:
            return 'Other OVN'


class DashboardGenerator:
    """Generate HTML dashboard from processed metrics data."""
    
    def __init__(self, title: str = "Kubernetes Workload Metrics Dashboard"):
        self.title = title
    
    def generate_html(self, pod_latency: Dict[str, Any], ovn_cpu: Dict[str, Any], 
                     ovn_memory: Dict[str, Any], output_file: str = "index.html") -> None:
        """Generate complete HTML dashboard."""
        
        # Convert data to JavaScript format
        js_pod_latency = json.dumps(pod_latency['data'])
        js_ovn_cpu = json.dumps(ovn_cpu)
        js_ovn_memory = json.dumps(ovn_memory)
        
        stats = pod_latency['stats']
        
        html_content = f"""<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>{self.title}</title>
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/date-fns@2.29.3/index.min.js"></script>
    <script src="https://cdn.jsdelivr.net/npm/chartjs-adapter-date-fns@2.0.1/dist/chartjs-adapter-date-fns.bundle.min.js"></script>
    {self._generate_css()}
</head>
<body>
    <div class="header">
        <h1>Kubernetes Workload Metrics</h1>
        <p>kubelet-density-cni Performance Dashboard</p>
        <div class="generated-info">
            Generated on {datetime.now().strftime('%Y-%m-%d %H:%M:%S UTC')}
        </div>
    </div>

    <div class="dashboard">
        <!-- Main KPI: Pod Ready Latency -->
        <div class="metric-card main-kpi">
            <h2>🎯 Pod Ready Latency (Main KPI)</h2>
            <div class="stats">
                <div class="stat">
                    <div class="stat-value">{stats.get('avg_latency', 0):.1f}</div>
                    <div class="stat-label">Avg Latency (ms)</div>
                </div>
                <div class="stat">
                    <div class="stat-value">{stats.get('max_latency', 0):.0f}</div>
                    <div class="stat-label">Max Latency (ms)</div>
                </div>
                <div class="stat">
                    <div class="stat-value">{stats.get('total_pods', 0)}</div>
                    <div class="stat-label">Total Pods</div>
                </div>
                <div class="stat">
                    <div class="stat-value">{self._format_time_range(stats.get('start_time'), stats.get('end_time'))}</div>
                    <div class="stat-label">Time Range</div>
                </div>
            </div>
            <div class="chart-container">
                <canvas id="podLatencyChart"></canvas>
            </div>
        </div>

        <!-- Secondary Metrics -->
        <div class="secondary-metrics">
            <!-- OVN CPU Usage -->
            <div class="metric-card">
                <h2>💻 OVN Container CPU Usage</h2>
                <div class="chart-container">
                    <canvas id="ovnCpuChart"></canvas>
                </div>
            </div>

            <!-- OVN Memory Usage -->
            <div class="metric-card">
                <h2>🧠 OVN Container Memory Usage</h2>
                <div class="chart-container">
                    <canvas id="ovnMemoryChart"></canvas>
                </div>
            </div>
        </div>
    </div>

    <script>
        // Embedded data
        const podLatencyData = {js_pod_latency};
        const ovnCpuData = {js_ovn_cpu};
        const ovnMemoryData = {js_ovn_memory};

        {self._generate_javascript()}
    </script>
</body>
</html>"""
        
        with open(output_file, 'w') as f:
            f.write(html_content)
        
        print(f"✓ Dashboard generated: {output_file}")
    
    def _format_time_range(self, start_time: Optional[str], end_time: Optional[str]) -> str:
        """Format time range for display."""
        if not start_time or not end_time:
            return "-"
        
        try:
            start = datetime.fromisoformat(start_time.replace('Z', '+00:00'))
            end = datetime.fromisoformat(end_time.replace('Z', '+00:00'))
            
            if start.date() == end.date():
                return f"{start.strftime('%m/%d/%Y')}"
            else:
                return f"{start.strftime('%m/%d')} - {end.strftime('%m/%d')}"
        except:
            return "-"
    
    def _generate_css(self) -> str:
        """Generate CSS styles for the dashboard."""
        return """<style>
        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            margin: 0;
            padding: 20px;
            background-color: #f5f5f5;
            line-height: 1.6;
        }
        
        .header {
            text-align: center;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            color: white;
            padding: 30px;
            border-radius: 10px;
            margin-bottom: 30px;
            box-shadow: 0 4px 15px rgba(0,0,0,0.1);
        }
        
        .header h1 {
            margin: 0;
            font-size: 2.5em;
            font-weight: 300;
        }
        
        .header p {
            margin: 10px 0 0 0;
            font-size: 1.2em;
            opacity: 0.9;
        }
        
        .generated-info {
            margin-top: 15px;
            font-size: 0.9em;
            opacity: 0.7;
        }
        
        .dashboard {
            display: grid;
            grid-template-columns: 1fr;
            gap: 30px;
            max-width: 1400px;
            margin: 0 auto;
        }
        
        .metric-card {
            background: white;
            border-radius: 10px;
            padding: 25px;
            box-shadow: 0 4px 15px rgba(0,0,0,0.1);
            transition: transform 0.2s;
        }
        
        .metric-card:hover {
            transform: translateY(-2px);
        }
        
        .metric-card h2 {
            margin: 0 0 20px 0;
            color: #333;
            font-size: 1.5em;
            font-weight: 500;
            border-bottom: 3px solid #667eea;
            padding-bottom: 10px;
        }
        
        .metric-card.main-kpi h2 {
            border-bottom-color: #e74c3c;
        }
        
        .chart-container {
            position: relative;
            height: 400px;
            margin: 20px 0;
        }
        
        .main-kpi .chart-container {
            height: 500px;
        }
        
        .stats {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(120px, 1fr));
            gap: 15px;
            margin: 20px 0;
        }
        
        .stat {
            text-align: center;
            padding: 15px;
            background: #f8f9fa;
            border-radius: 8px;
            border-left: 4px solid #667eea;
        }
        
        .stat-value {
            font-size: 1.5em;
            font-weight: bold;
            color: #333;
        }
        
        .stat-label {
            font-size: 0.9em;
            color: #666;
            margin-top: 5px;
        }
        
        .secondary-metrics {
            display: grid;
            grid-template-columns: repeat(auto-fit, minmax(600px, 1fr));
            gap: 30px;
        }
        
        .error {
            background: #f8d7da;
            color: #721c24;
            padding: 15px;
            border-radius: 5px;
            margin: 20px 0;
            border: 1px solid #f5c6cb;
        }
        
        @media (max-width: 768px) {
            .dashboard {
                padding: 10px;
            }
            
            .secondary-metrics {
                grid-template-columns: 1fr;
            }
            
            .stats {
                grid-template-columns: repeat(2, 1fr);
            }
        }
    </style>"""
    
    def _generate_javascript(self) -> str:
        """Generate JavaScript code for chart visualization."""
        return """
        // Chart.js configuration
        Chart.defaults.font.family = "'Segoe UI', Tahoma, Geneva, Verdana, sans-serif";
        Chart.defaults.color = '#333';

        let podLatencyChart, ovnCpuChart, ovnMemoryChart;

        // Initialize charts
        function initCharts() {
            createPodLatencyChart();
            createOvnCpuChart();
            createOvnMemoryChart();
        }

        // Create pod latency chart
        function createPodLatencyChart() {
            const ctx = document.getElementById('podLatencyChart').getContext('2d');
            
            const chartData = podLatencyData.map(d => ({
                x: new Date(d.timestamp),
                y: d.podReadyLatency,
                pod: d.podName,
                node: d.nodeName
            })).sort((a, b) => a.x - b.x);

            podLatencyChart = new Chart(ctx, {
                type: 'line',
                data: {
                    datasets: [{
                        label: 'Pod Ready Latency',
                        data: chartData,
                        borderColor: '#e74c3c',
                        backgroundColor: 'rgba(231, 76, 60, 0.1)',
                        borderWidth: 2,
                        fill: true,
                        pointRadius: 3,
                        pointHoverRadius: 5,
                        tension: 0.1
                    }]
                },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    scales: {
                        x: {
                            type: 'time',
                            time: {
                                displayFormats: {
                                    minute: 'HH:mm',
                                    hour: 'HH:mm',
                                    day: 'MMM dd'
                                }
                            },
                            title: {
                                display: true,
                                text: 'Time'
                            }
                        },
                        y: {
                            beginAtZero: true,
                            title: {
                                display: true,
                                text: 'Latency (ms)'
                            }
                        }
                    },
                    plugins: {
                        tooltip: {
                            callbacks: {
                                label: function(context) {
                                    return [
                                        `Latency: ${context.parsed.y}ms`,
                                        `Pod: ${context.raw.pod}`,
                                        `Node: ${context.raw.node}`
                                    ];
                                }
                            }
                        },
                        legend: {
                            display: false
                        }
                    }
                }
            });
        }

        // Create OVN CPU chart
        function createOvnCpuChart() {
            const ctx = document.getElementById('ovnCpuChart').getContext('2d');
            
            const colors = ['#3498db', '#2ecc71', '#f39c12', '#9b59b6', '#e67e22'];
            const datasets = Object.keys(ovnCpuData).map((containerType, index) => ({
                label: containerType,
                data: ovnCpuData[containerType].map(d => ({
                    x: new Date(d.timestamp),
                    y: d.value,
                    pod: d.pod,
                    node: d.node
                })).sort((a, b) => a.x - b.x),
                borderColor: colors[index % colors.length],
                backgroundColor: colors[index % colors.length] + '20',
                borderWidth: 2,
                fill: false,
                pointRadius: 2,
                tension: 0.1
            }));

            ovnCpuChart = new Chart(ctx, {
                type: 'line',
                data: { datasets },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    scales: {
                        x: {
                            type: 'time',
                            time: {
                                displayFormats: {
                                    minute: 'HH:mm',
                                    hour: 'HH:mm'
                                }
                            },
                            title: {
                                display: true,
                                text: 'Time'
                            }
                        },
                        y: {
                            beginAtZero: true,
                            title: {
                                display: true,
                                text: 'CPU Usage (%)'
                            }
                        }
                    },
                    plugins: {
                        tooltip: {
                            callbacks: {
                                label: function(context) {
                                    return [
                                        `${context.dataset.label}: ${context.parsed.y.toFixed(2)}%`,
                                        `Pod: ${context.raw.pod}`,
                                        `Node: ${context.raw.node}`
                                    ];
                                }
                            }
                        }
                    }
                }
            });
        }

        // Create OVN Memory chart
        function createOvnMemoryChart() {
            const ctx = document.getElementById('ovnMemoryChart').getContext('2d');
            
            const colors = ['#e74c3c', '#34495e', '#16a085', '#f1c40f', '#8e44ad'];
            const datasets = Object.keys(ovnMemoryData).map((containerType, index) => ({
                label: containerType,
                data: ovnMemoryData[containerType].map(d => ({
                    x: new Date(d.timestamp),
                    y: d.value,
                    pod: d.pod,
                    node: d.node
                })).sort((a, b) => a.x - b.x),
                borderColor: colors[index % colors.length],
                backgroundColor: colors[index % colors.length] + '20',
                borderWidth: 2,
                fill: false,
                pointRadius: 2,
                tension: 0.1
            }));

            ovnMemoryChart = new Chart(ctx, {
                type: 'line',
                data: { datasets },
                options: {
                    responsive: true,
                    maintainAspectRatio: false,
                    scales: {
                        x: {
                            type: 'time',
                            time: {
                                displayFormats: {
                                    minute: 'HH:mm',
                                    hour: 'HH:mm'
                                }
                            },
                            title: {
                                display: true,
                                text: 'Time'
                            }
                        },
                        y: {
                            beginAtZero: true,
                            title: {
                                display: true,
                                text: 'Memory Usage (MB)'
                            }
                        }
                    },
                    plugins: {
                        tooltip: {
                            callbacks: {
                                label: function(context) {
                                    return [
                                        `${context.dataset.label}: ${context.parsed.y.toFixed(2)} MB`,
                                        `Pod: ${context.raw.pod}`,
                                        `Node: ${context.raw.node}`
                                    ];
                                }
                            }
                        }
                    }
                }
            });
        }

        // Initialize when page loads
        document.addEventListener('DOMContentLoaded', initCharts);
        """


def main():
    """Main function to generate the dashboard."""
    parser = argparse.ArgumentParser(description='Generate Kubernetes workload metrics dashboard')
    parser.add_argument('--metrics-dir', default='.', 
                       help='Directory containing JSON metrics files (default: current directory)')
    parser.add_argument('--output', default='index.html', 
                       help='Output HTML file name (default: index.html)')
    parser.add_argument('--title', default='Kubernetes Workload Metrics Dashboard',
                       help='Dashboard title')
    
    args = parser.parse_args()
    
    print(f"🚀 Generating Kubernetes Workload Metrics Dashboard")
    print(f"📁 Metrics directory: {args.metrics_dir}")
    print(f"📄 Output file: {args.output}")
    print()
    
    # Initialize processor and generator
    processor = MetricsProcessor(args.metrics_dir)
    generator = DashboardGenerator(args.title)
    
    # Load and process data
    print("📊 Loading and processing metrics data...")
    
    # Process pod latency data (main KPI)
    pod_latency_raw = processor.load_json_file(processor.pod_latency_file)
    pod_latency_processed = processor.process_pod_latency_data(pod_latency_raw)
    
    # Process OVN CPU data
    pod_cpu_raw = processor.load_json_file(processor.pod_cpu_file)
    ovn_cpu_processed = processor.process_ovn_data(pod_cpu_raw, 'cpu')
    
    # Process OVN Memory data
    pod_memory_raw = processor.load_json_file(processor.pod_memory_file)
    ovn_memory_processed = processor.process_ovn_data(pod_memory_raw, 'memory')
    
    print()
    print("📈 Data Processing Summary:")
    print(f"   Pod Latency Records: {len(pod_latency_processed['data'])}")
    print(f"   OVN CPU Container Types: {len(ovn_cpu_processed)}")
    print(f"   OVN Memory Container Types: {len(ovn_memory_processed)}")
    
    if pod_latency_processed['stats']:
        stats = pod_latency_processed['stats']
        print(f"   Average Pod Ready Latency: {stats['avg_latency']:.1f}ms")
        print(f"   Max Pod Ready Latency: {stats['max_latency']:.0f}ms")
    
    print()
    
    # Generate HTML dashboard
    print("🎨 Generating HTML dashboard...")
    generator.generate_html(
        pod_latency_processed, 
        ovn_cpu_processed, 
        ovn_memory_processed,
        args.output
    )
    
    print()
    print("🎉 Dashboard generation complete!")
    print(f"📱 Open {args.output} in your browser to view the dashboard")
    print("🚀 Ready for GitHub Pages deployment!")


if __name__ == "__main__":
    main()
